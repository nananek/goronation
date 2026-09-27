package webauthn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

const (
	testRPID   = "goro.example.ts.net"
	testOrigin = "https://goro.example.ts.net"
)

func testConfig() Config { return Config{RPID: testRPID, RPName: "goro test", Origin: testOrigin} }

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// fakeAuthenticator は、テストのための、偽のプラットフォーム認証器 (ES256 の鍵ペアを持ち、
// attestationObject・authenticatorData・署名を、本物のブラウザ/認証器が作るのと同じ形で組み立てる)。
type fakeAuthenticator struct {
	priv      *ecdsa.PrivateKey
	credID    []byte
	signCount uint32
}

func newFakeAuthenticator(t *testing.T) *fakeAuthenticator {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := make([]byte, 16)
	if _, err := rand.Read(credID); err != nil {
		t.Fatal(err)
	}
	return &fakeAuthenticator{priv: priv, credID: credID}
}

// buildAuthData は、authenticatorData を組み立てる。attested が true なら、attestedCredentialData
// (aaguid・credentialId・credentialPublicKey) を足す (登録のとき)。
func (a *fakeAuthenticator) buildAuthData(t *testing.T, rpID string, attested, up bool) []byte {
	t.Helper()
	h := sha256.Sum256([]byte(rpID))
	buf := append([]byte{}, h[:]...)
	var flags byte
	if up {
		flags |= flagUP
	}
	if attested {
		flags |= flagAT
	}
	buf = append(buf, flags)
	buf = append(buf, byte(a.signCount>>24), byte(a.signCount>>16), byte(a.signCount>>8), byte(a.signCount))
	if !attested {
		return buf
	}
	buf = append(buf, make([]byte, 16)...) // aaguid (使わない)
	buf = append(buf, byte(len(a.credID)>>8), byte(len(a.credID)))
	buf = append(buf, a.credID...)
	key := coseEC2Key{
		Kty: coseKtyEC2, Alg: coseAlgES256, Crv: coseCrvP256,
		X: a.priv.PublicKey.X.FillBytes(make([]byte, 32)), Y: a.priv.PublicKey.Y.FillBytes(make([]byte, 32)),
	}
	keyBytes, err := cbor.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	return append(buf, keyBytes...)
}

func b64s(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// register は、opts.Challenge に対する、本物らしい AttestationResponse を組み立てる。
func (a *fakeAuthenticator) register(t *testing.T, rpID, origin string, challenge string) AttestationResponse {
	t.Helper()
	authData := a.buildAuthData(t, rpID, true, true)
	attObj, err := cbor.Marshal(struct {
		Fmt      string         `cbor:"fmt"`
		AttStmt  map[string]any `cbor:"attStmt"`
		AuthData []byte         `cbor:"authData"`
	}{"none", map[string]any{}, authData})
	if err != nil {
		t.Fatal(err)
	}
	cdJSON, err := json.Marshal(clientData{Type: "webauthn.create", Challenge: challenge, Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	var resp AttestationResponse
	resp.ID = b64s(a.credID)
	resp.Response.ClientDataJSON = b64s(cdJSON)
	resp.Response.AttestationObject = b64s(attObj)
	return resp
}

// authenticate は、opts.Challenge に対する、本物らしい AssertionResponse を組み立てる (signCount を進める)。
func (a *fakeAuthenticator) authenticate(t *testing.T, rpID, origin string, challenge string) AssertionResponse {
	t.Helper()
	a.signCount++
	authData := a.buildAuthData(t, rpID, false, true)
	cdJSON, err := json.Marshal(clientData{Type: "webauthn.get", Challenge: challenge, Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	cdHash := sha256.Sum256(cdJSON)
	signed := append(append([]byte{}, authData...), cdHash[:]...)
	hh := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, a.priv, hh[:])
	if err != nil {
		t.Fatal(err)
	}
	var resp AssertionResponse
	resp.ID = b64s(a.credID)
	resp.Response.ClientDataJSON = b64s(cdJSON)
	resp.Response.AuthenticatorData = b64s(authData)
	resp.Response.Signature = b64s(sig)
	return resp
}

// register は、bootstrap token を発行してから登録まで一気に行う (多くのテストの前提として使う)。
func mustRegister(t *testing.T, ctx context.Context, cfg Config, st *Store, a *fakeAuthenticator) {
	t.Helper()
	tok, err := IssueBootstrapToken(ctx, st, time.Minute)
	if err != nil {
		t.Fatalf("IssueBootstrapToken: %v", err)
	}
	opts, state, err := RegisterBegin(ctx, cfg, st, tok)
	if err != nil {
		t.Fatalf("RegisterBegin: %v", err)
	}
	resp := a.register(t, cfg.RPID, cfg.Origin, opts.Challenge)
	if err := RegisterFinish(ctx, cfg, st, state, resp); err != nil {
		t.Fatalf("RegisterFinish: %v", err)
	}
}

func TestFullRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)

	mustRegister(t, ctx, cfg, st, a)

	opts, state, err := AuthenticateBegin(ctx, cfg, st)
	if err != nil {
		t.Fatalf("AuthenticateBegin: %v", err)
	}
	if len(opts.AllowCredentials) != 1 || opts.AllowCredentials[0].ID != b64s(a.credID) {
		t.Fatalf("AllowCredentials = %+v", opts.AllowCredentials)
	}
	resp := a.authenticate(t, cfg.RPID, cfg.Origin, opts.Challenge)
	session, err := AuthenticateFinish(ctx, cfg, st, state, resp)
	if err != nil {
		t.Fatalf("AuthenticateFinish: %v", err)
	}
	if err := VerifySession(ctx, st, session); err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
}

func TestRegisterRequiresValidBootstrapToken(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	if _, _, err := RegisterBegin(ctx, cfg, st, "not-a-real-token"); err != ErrBadBootstrapToken {
		t.Fatalf("RegisterBegin (トークン無し) = %v, want ErrBadBootstrapToken", err)
	}
	tok, err := IssueBootstrapToken(ctx, st, -time.Minute) // すでに期限切れ
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RegisterBegin(ctx, cfg, st, tok); err != ErrBadBootstrapToken {
		t.Fatalf("RegisterBegin (期限切れ) = %v, want ErrBadBootstrapToken", err)
	}
}

func TestRegisterRefusesWhenAlreadyRegistered(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	mustRegister(t, ctx, cfg, st, a)

	tok, err := IssueBootstrapToken(ctx, st, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RegisterBegin(ctx, cfg, st, tok); err != ErrAlreadyRegistered {
		t.Fatalf("2 回目の RegisterBegin = %v, want ErrAlreadyRegistered", err)
	}
}

func TestRegisterFinishRejectsWrongOrigin(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	tok, err := IssueBootstrapToken(ctx, st, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	opts, state, err := RegisterBegin(ctx, cfg, st, tok)
	if err != nil {
		t.Fatal(err)
	}
	resp := a.register(t, cfg.RPID, "https://evil.example", opts.Challenge) // 偽の origin
	if err := RegisterFinish(ctx, cfg, st, state, resp); err == nil {
		t.Fatal("偽の origin なのに RegisterFinish が成功した")
	}
}

func TestRegisterFinishRejectsWrongRPID(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	tok, err := IssueBootstrapToken(ctx, st, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	opts, state, err := RegisterBegin(ctx, cfg, st, tok)
	if err != nil {
		t.Fatal(err)
	}
	resp := a.register(t, "evil.example", cfg.Origin, opts.Challenge) // rpIdHash が合わない
	if err := RegisterFinish(ctx, cfg, st, state, resp); err == nil {
		t.Fatal("rpIdHash が違うのに RegisterFinish が成功した")
	}
}

func TestRegisterFinishRejectsReplayedChallenge(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	tok, err := IssueBootstrapToken(ctx, st, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, state, err := RegisterBegin(ctx, cfg, st, tok)
	if err != nil {
		t.Fatal(err)
	}
	resp := a.register(t, cfg.RPID, cfg.Origin, b64s([]byte("some-other-challenge-not-issued")))
	if err := RegisterFinish(ctx, cfg, st, state, resp); err == nil {
		t.Fatal("state の challenge と違う challenge なのに RegisterFinish が成功した")
	}
}

func TestAuthenticateBeginRequiresRegistration(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	if _, _, err := AuthenticateBegin(ctx, cfg, st); err != ErrNotRegistered {
		t.Fatalf("AuthenticateBegin (未登録) = %v, want ErrNotRegistered", err)
	}
}

func TestAuthenticateFinishRejectsTamperedSignature(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	mustRegister(t, ctx, cfg, st, a)

	opts, state, err := AuthenticateBegin(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	resp := a.authenticate(t, cfg.RPID, cfg.Origin, opts.Challenge)
	// 署名を壊す (1 バイト反転)。
	sig, err := base64.RawURLEncoding.DecodeString(resp.Response.Signature)
	if err != nil {
		t.Fatal(err)
	}
	sig[0] ^= 0xff
	resp.Response.Signature = b64s(sig)
	if _, err := AuthenticateFinish(ctx, cfg, st, state, resp); err == nil {
		t.Fatal("署名を改ざんしたのに AuthenticateFinish が成功した")
	}
}

func TestAuthenticateFinishRejectsAnotherKeysSignature(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	mustRegister(t, ctx, cfg, st, a)

	other := newFakeAuthenticator(t)
	other.credID = a.credID // 同じ credential id を名乗るが、別の秘密鍵で署名する

	opts, state, err := AuthenticateBegin(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	resp := other.authenticate(t, cfg.RPID, cfg.Origin, opts.Challenge)
	if _, err := AuthenticateFinish(ctx, cfg, st, state, resp); err == nil {
		t.Fatal("別の秘密鍵の署名なのに AuthenticateFinish が成功した")
	}
}

func TestAuthenticateFinishRejectsUnknownCredentialID(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	mustRegister(t, ctx, cfg, st, a)

	stranger := newFakeAuthenticator(t) // 登録されていない、別の credential id
	opts, state, err := AuthenticateBegin(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	resp := stranger.authenticate(t, cfg.RPID, cfg.Origin, opts.Challenge)
	if _, err := AuthenticateFinish(ctx, cfg, st, state, resp); err == nil {
		t.Fatal("登録されていない credential id なのに AuthenticateFinish が成功した")
	}
}

func TestVerifySessionRejectsTamperedToken(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	mustRegister(t, ctx, cfg, st, a)

	opts, state, err := AuthenticateBegin(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	resp := a.authenticate(t, cfg.RPID, cfg.Origin, opts.Challenge)
	session, err := AuthenticateFinish(ctx, cfg, st, state, resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySession(ctx, st, session); err != nil {
		t.Fatalf("正しいセッションが拒否された: %v", err)
	}
	// 署名部分を 1 文字だけ変える。
	tampered := session[:len(session)-1] + map[byte]string{'a': "b", 'b': "a"}[session[len(session)-1]]
	if tampered == session {
		tampered = session + "x"
	}
	if err := VerifySession(ctx, st, tampered); err == nil {
		t.Fatal("改ざんしたセッション token が受理された")
	}
	if err := VerifySession(ctx, st, "not.a.token"); err == nil {
		t.Fatal("形の壊れた token が受理された")
	}
}

func TestVerifySessionRejectsStateTokenAsSession(t *testing.T) {
	// state token (challenge を運ぶもの) を、セッション token として使い回せないこと (purpose の分離)。
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	mustRegister(t, ctx, cfg, st, a)

	_, state, err := AuthenticateBegin(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySession(ctx, st, state); err == nil {
		t.Fatal("state token が、セッション token として受理された")
	}
}

func TestAuthenticateFinishRejectsExpiredChallenge(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	st := testStore(t)
	a := newFakeAuthenticator(t)
	mustRegister(t, ctx, cfg, st, a)

	persisted, err := st.load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := randomBytes(32)
	if err != nil {
		t.Fatal(err)
	}
	expired := signToken(persisted.SessionSecret, mustJSON(stateClaims{
		Purpose: "authenticate", Challenge: challenge, Expiry: time.Now().Add(-time.Second).Unix(),
	}))
	resp := a.authenticate(t, cfg.RPID, cfg.Origin, b64s(challenge))
	if _, err := AuthenticateFinish(ctx, cfg, st, expired, resp); err == nil {
		t.Fatal("期限切れの state token なのに AuthenticateFinish が成功した")
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{RPID: "example.com", Origin: "https://example.com"}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid.Validate() = %v", err)
	}
	localhost := Config{RPID: "localhost", Origin: "http://localhost:8080"}
	if err := localhost.Validate(); err != nil {
		t.Errorf("localhost.Validate() = %v", err)
	}
	for name, cfg := range map[string]Config{
		"RPID 空":          {RPID: "", Origin: "https://example.com"},
		"Origin が http":   {RPID: "example.com", Origin: "http://example.com"},
		"RPID と host が違う": {RPID: "other.example", Origin: "https://example.com"},
		"Origin に path":   {RPID: "example.com", Origin: "https://example.com/x"},
		"Origin が壊れている":   {RPID: "example.com", Origin: "://not a url"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate が通ってしまった: %+v", name, cfg)
		}
	}
}

func TestParseCOSEPublicKeyRejectsWrongAlgorithm(t *testing.T) {
	key := coseEC2Key{Kty: coseKtyEC2, Alg: -257 /* RS256 */, Crv: coseCrvP256, X: make([]byte, 32), Y: make([]byte, 32)}
	b, err := cbor.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCOSEPublicKey(b); err == nil {
		t.Fatal("ES256 でないのに parseCOSEPublicKey が成功した")
	}
}

func TestParseCOSEPublicKeyRejectsTrailingBytes(t *testing.T) {
	key := coseEC2Key{Kty: coseKtyEC2, Alg: coseAlgES256, Crv: coseCrvP256, X: make([]byte, 32), Y: make([]byte, 32)}
	b, err := cbor.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, 0x00)
	if _, err := parseCOSEPublicKey(b); err == nil {
		t.Fatal("余分なバイトがあるのに parseCOSEPublicKey が成功した")
	}
}

func TestParseAuthenticatorDataRejectsExtensions(t *testing.T) {
	h := sha256.Sum256([]byte(testRPID))
	buf := append([]byte{}, h[:]...)
	buf = append(buf, flagUP|flagED) // AT なし・ED あり
	buf = append(buf, 0, 0, 0, 0)
	if _, err := parseAuthenticatorData(buf); err == nil {
		t.Fatal("拡張データがあるのに parseAuthenticatorData が成功した")
	}
}

func TestParseAuthenticatorDataRejectsTooShort(t *testing.T) {
	if _, err := parseAuthenticatorData(make([]byte, 10)); err == nil {
		t.Fatal("短すぎる authData なのに parseAuthenticatorData が成功した")
	}
}

func TestSignTokenVerifyTokenRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok := signToken(secret, []byte(`{"a":1}`))
	got, err := verifyToken(secret, tok)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("verifyToken = %q, %v", got, err)
	}
	if _, err := verifyToken([]byte("different-secret-xxxxxxxxxxxxxxx"), tok); err != ErrBadToken {
		t.Errorf("別の secret で検証できてしまった: %v", err)
	}
	if _, err := verifyToken(secret, "garbage"); err != ErrBadToken {
		t.Errorf("形の壊れた token: %v", err)
	}
	if _, err := verifyToken(secret, strings.Replace(tok, ".", "X", 1)); err == nil {
		t.Error("壊した token が受理された")
	}
}

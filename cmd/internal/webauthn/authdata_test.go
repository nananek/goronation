package webauthn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/nananek/goronation/cmd/internal/credfile"
	"github.com/nananek/goronation/core/credential"
)

func header(flags byte) []byte {
	h := sha256.Sum256([]byte(testRPID))
	return append(append(append([]byte{}, h[:]...), flags), 0, 0, 0, 1)
}

func cborOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := cbor.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// attestedData は、AT フラグつきの authData の本体 (aaguid・credentialId・COSE 鍵)。
func attestedData(t *testing.T, a *fakeAuthenticator) []byte {
	t.Helper()
	full := a.buildAuthData(t, testRPID, true, true)
	return full[37:]
}

func TestParseAuthenticatorDataExtensions(t *testing.T) {
	a := newFakeAuthenticator(t)
	key := attestedData(t, a)
	hs := cborOf(t, map[string]any{"hmac-secret": true})
	for name, tc := range map[string]struct {
		b    []byte
		ok   bool
		keys []string
	}{
		"assertion + hmac-secret true":    {append(header(flagUP|flagED), hs...), true, []string{"hmac-secret"}},
		"assertion + hmac-secret bytes":   {append(header(flagUP|flagED), cborOf(t, map[string]any{"hmac-secret": bytes.Repeat([]byte{1}, 80)})...), true, []string{"hmac-secret"}},
		"attested + ext":                  {append(append(header(flagUP|flagAT|flagED), key...), cborOf(t, map[string]any{"hmac-secret": true, "credProtect": 2})...), true, []string{"credProtect", "hmac-secret"}},
		"attested, no ext":                {append(header(flagUP|flagAT), key...), true, nil},
		"ED but nothing follows":          {header(flagUP | flagED), false, nil},
		"ED but empty map":                {append(header(flagUP|flagED), 0xa0), false, nil},
		"ED but not a map":                {append(header(flagUP|flagED), 0x01), false, nil},
		"trailing bytes after ext":        {append(append(header(flagUP|flagED), hs...), 0x00), false, nil},
		"no ED but bytes follow":          {append(header(flagUP), hs...), false, nil},
		"attested no ED but bytes follow": {append(append(header(flagUP|flagAT), key...), hs...), false, nil},
		"unknown extension":               {append(header(flagUP|flagED), cborOf(t, map[string]any{"largeBlob": true})...), false, nil},
		"hmac-secret wrong type":          {append(header(flagUP|flagED), cborOf(t, map[string]any{"hmac-secret": "yes"})...), false, nil},
		"hmac-secret too long":            {append(header(flagUP|flagED), cborOf(t, map[string]any{"hmac-secret": bytes.Repeat([]byte{1}, 129)})...), false, nil},
		"credProtect wrong type":          {append(header(flagUP|flagED), cborOf(t, map[string]any{"credProtect": true})...), false, nil},
		"too many entries":                {append(header(flagUP|flagED), cborOf(t, map[string]any{"hmac-secret": true, "credProtect": 1, "a": 1, "b": 2, "c": 3})...), false, nil},
		"duplicate key":                   {append(header(flagUP|flagED), 0xa2, 0x6b, 'h', 'm', 'a', 'c', '-', 's', 'e', 'c', 'r', 'e', 't', 0xf5, 0x6b, 'h', 'm', 'a', 'c', '-', 's', 'e', 'c', 'r', 'e', 't', 0xf5), false, nil},
		"deeply nested value":             {append(header(flagUP|flagED), 0xa1, 0x6b, 'h', 'm', 'a', 'c', '-', 's', 'e', 'c', 'r', 'e', 't', 0x81, 0x81, 0x81, 0x81, 0x81, 0x81, 0x01), false, nil},
		"truncated map":                   {append(header(flagUP|flagED), 0xa1, 0x6b, 'h'), false, nil},
		"attested: key cbor truncated":    {append(header(flagUP|flagAT), key[:len(key)-5]...), false, nil},
	} {
		got, err := parseAuthenticatorData(tc.b)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
			continue
		}
		if tc.ok && !bytes.Equal([]byte(joinKeys(got.Extensions)), []byte(joinKeys(tc.keys))) {
			t.Errorf("%s: extensions = %v, want %v", name, got.Extensions, tc.keys)
		}
		if tc.ok && name == "attested + ext" {
			if _, err := parseCOSEPublicKey(got.CredentialPublicKey); err != nil {
				t.Errorf("COSE 鍵が、拡張データの手前で切れていない: %v", err)
			}
		}
	}
}

func joinKeys(k []string) string {
	out := ""
	for _, s := range k {
		out += s + ","
	}
	return out
}

// 拡張データつきの登録・認証が、セレモニー全体で通る。
func TestCeremonyAcceptsExtensionData(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	tok, _ := IssueBootstrapToken(ctx, st, time.Minute)
	opts, state, _ := RegisterBegin(ctx, testConfig(), st, tok)
	a := newFakeAuthenticator(t)
	authData := append(a.buildAuthData(t, testRPID, true, true), cborOf(t, map[string]any{"hmac-secret": true})...)
	authData[32] |= flagED
	attObj := cborOf(t, struct {
		Fmt      string         `cbor:"fmt"`
		AttStmt  map[string]any `cbor:"attStmt"`
		AuthData []byte         `cbor:"authData"`
	}{"none", map[string]any{}, authData})
	cd, _ := json.Marshal(clientData{Type: "webauthn.create", Challenge: opts.Challenge, Origin: testOrigin})
	var resp AttestationResponse
	resp.ID = b64s(a.credID)
	resp.Response.ClientDataJSON = b64s(cd)
	resp.Response.AttestationObject = b64s(attObj)
	if err := RegisterFinish(ctx, testConfig(), st, state, resp); err != nil {
		t.Fatalf("拡張データつきの登録: %v", err)
	}
}

// 単数だった版の保存形式 (credential) を、読んで、複数の形 (credentials) に移す。書き戻すと、単数の形は残らない。
func TestLoadMigratesSingleCredentialFormat(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cf, err := credfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"credential":{"id":"AQID","x":"eA==","y":"eQ==","sign_count":3,"rp_id":"h"},"session_secret":"c2VjcmV0"}`
	if err := cf.Save(stateName, credential.New(legacy)); err != nil {
		t.Fatal(err)
	}
	st, _ := NewStore(dir)
	got, err := st.load(ctx)
	if err != nil || len(got.Credentials) != 1 || !bytes.Equal(got.Credentials[0].ID, []byte{1, 2, 3}) || got.Credentials[0].SignCount != 3 || got.Credential != nil {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if err := st.save(got); err != nil {
		t.Fatal(err)
	}
	sec, _ := cf.Token(ctx, stateName)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sec.Reveal()), &raw); err != nil {
		t.Fatal(err)
	}
	if _, old := raw["credential"]; old || raw["credentials"] == nil {
		t.Fatalf("保存の形 = %v", raw)
	}
}

// 上限 (17 個) の保存された状態は、読まない。
func TestLoadRejectsTooManyCredentials(t *testing.T) {
	st := testStore(t)
	s := persistedState{SessionSecret: []byte("x")}
	for i := 0; i < maxCredentials+1; i++ {
		s.Credentials = append(s.Credentials, storedCredential{ID: []byte{byte(i)}, PublicKeyX: []byte{1}, PublicKeyY: []byte{1}, RPID: "h"})
	}
	if err := st.save(s); err != nil {
		t.Fatal(err)
	}
	if _, err := st.load(context.Background()); err == nil {
		t.Fatal("上限を超えた状態を、読めた")
	}
}

// 認可・登録の期限が切れたら、CommitAdd・RemoveCredential は断る。
func TestExpiredAuthorizationIsRejected(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := persistedState{SessionSecret: []byte("x"), Credentials: []storedCredential{
		{ID: []byte("a"), PublicKeyX: []byte{1}, PublicKeyY: []byte{1}, RPID: "h"},
		{ID: []byte("b"), PublicKeyX: []byte{1}, PublicKeyY: []byte{1}, RPID: "h"},
	}}
	if err := st.save(s); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-challengeTTL - time.Second)
	authz := &Assertion{cred: []byte("a"), op: string(OpRemoveCredential), target: []byte("b"), at: old}
	if err := RemoveCredential(ctx, st, []byte("b"), authz); err == nil {
		t.Error("期限切れの認可で、削除できた")
	}
	authz.at = time.Now()
	authz.cred = []byte("zzz") // 認可した passkey が、もう無い
	if err := RemoveCredential(ctx, st, []byte("b"), authz); err == nil {
		t.Error("存在しない passkey の認可で、削除できた")
	}
	authz.cred = []byte("a")
	if err := RemoveCredential(ctx, st, []byte("b"), authz); err != nil {
		t.Errorf("有効な認可: %v", err)
	}
	cand := &Candidate{CredentialID: []byte("c"), requestID: "r", at: time.Now().Add(-3 * challengeTTL)}
	add := &Assertion{cred: []byte("a"), op: string(OpAddCredential), target: []byte("c"), requestID: "r", at: time.Now()}
	if err := CommitAdd(ctx, st, cand, add, ""); err == nil {
		t.Error("期限切れの候補を、保存できた")
	}
}

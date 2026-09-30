// Package webauthntest は、webauthn package の結合テスト用の、偽のプラットフォーム認証器 (ES256 の鍵ペア
// を持ち、attestationObject・authenticatorData・署名を、本物のブラウザ/認証器と同じ形で組み立てる)。
//
// webauthn 自身の CBOR デコード (ADR 0004 の、依存ゼロの方針の例外) を検証する側から、あえて独立に組み立てる
// (webauthn の内部関数を呼ばない): 検証する側とされる側が、同じ組み立てのバグを共有しないようにするため。
// webauthn.AttestationResponse・AssertionResponse という、webauthn 自身が公開する形だけを返すので、呼び手
// (他の package の結合テスト) は、webauthn の中身を知らなくてよい。
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/fxamacker/cbor/v2"
	"github.com/nananek/goronation/cmd/internal/webauthn"
)

// authData のフラグ (WebAuthn L2, §6.1)。webauthn package の中身と、値を揃えている。
const (
	flagUP = 1 << 0
	flagUV = 1 << 2
	flagAT = 1 << 6
	flagED = 1 << 7
)

var b64 = base64.RawURLEncoding

// Authenticator は、1 つの ES256 鍵ペアを持つ、偽の認証器。
type Authenticator struct {
	priv      *ecdsa.PrivateKey
	credID    []byte
	signCount uint32
	prfSecret []byte // 認証器の中の、PRF の秘密 (hmac-secret の CredRandom)。外には出ない。
}

// Options は、偽の認証器の振る舞い (偽装を含む)。ゼロ値は、UV なし・PRF なし・拡張データなし。
type Options struct {
	// UV は、authenticatorData の user verified フラグを立てる。
	UV bool
	// Salt があれば、認証器は、その salt での本物の PRF の出力を、clientExtensionResults (prf.results.first) に入れる
	// (登録時は、prf.enabled も立てる)。
	Salt []byte
	// FakePRF があれば、Salt の有無に関わらず、この値を、prf.results.first として送る (クライアントが、署名の外にある
	// 値を偽装する攻撃。ADR 0033 決定 5)。
	FakePRF []byte
	// PRFEnabled は、登録時に prf.enabled を立てる (Salt があれば、自動で立つ)。
	PRFEnabled bool
	// ExtData は、authenticatorData に、拡張データ (hmac-secret) を付け、ED フラグを立てる。
	ExtData bool
	// RawExtData があれば、ExtData の代わりに、この CBOR のバイト列を、拡張データとして付ける (不正な拡張のテスト用)。
	RawExtData []byte
}

// New は、新しい鍵ペアと credential id を持つ Authenticator を作る。
func New() (*Authenticator, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webauthntest: 鍵を作れない: %w", err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("webauthntest: 乱数を得られない: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("webauthntest: 乱数を得られない: %w", err)
	}
	return &Authenticator{priv: priv, credID: id, prfSecret: secret}, nil
}

// PRF は、salt に対する、この認証器の本物の PRF の出力 (WebAuthn の prf 拡張: 入力は SHA-256("WebAuthn PRF" || 0x00 || salt)、
// 出力は、それを hmac-secret の CredRandom で HMAC-SHA-256 した値)。同じ salt なら、いつも同じ値。
func (a *Authenticator) PRF(salt []byte) []byte {
	in := sha256.Sum256(append([]byte("WebAuthn PRF\x00"), salt...))
	m := hmac.New(sha256.New, a.prfSecret)
	m.Write(in[:])
	return m.Sum(nil)
}

// extensions は、clientExtensionResults を組み立てる。
func (a *Authenticator) extensions(o Options, registering bool) webauthn.ClientExtensionResults {
	var out webauthn.ClientExtensionResults
	var first []byte
	switch {
	case o.FakePRF != nil:
		first = o.FakePRF
	case o.Salt != nil:
		first = a.PRF(o.Salt)
	}
	if first == nil && !(registering && o.PRFEnabled) {
		return out
	}
	out.PRF = &webauthn.PRFResult{Enabled: registering && (o.PRFEnabled || o.Salt != nil)}
	if first != nil {
		out.PRF.Results = &struct {
			First string `json:"first"`
		}{First: b64.EncodeToString(first)}
	}
	return out
}

// CredentialID は、この認証器の credential id。
func (a *Authenticator) CredentialID() []byte { return a.credID }

// UseCredentialID は、a が名乗る credential id を、id に差し替える (別の credential になりすます攻撃の
// テスト用。署名は、引き続き a 自身の秘密鍵で行う)。
func (a *Authenticator) UseCredentialID(id []byte) { a.credID = id }

type coseEC2Key struct {
	Kty int    `cbor:"1,keyasint"`
	Alg int    `cbor:"3,keyasint"`
	Crv int    `cbor:"-1,keyasint"`
	X   []byte `cbor:"-2,keyasint"`
	Y   []byte `cbor:"-3,keyasint"`
}

func (a *Authenticator) authData(rpID string, attested bool, o Options) ([]byte, error) {
	h := sha256.Sum256([]byte(rpID))
	buf := append([]byte{}, h[:]...)
	flags := byte(flagUP)
	if attested {
		flags |= flagAT
	}
	if o.UV {
		flags |= flagUV
	}
	ext := o.RawExtData
	if ext == nil && o.ExtData {
		var err error
		if ext, err = cbor.Marshal(map[string]any{"hmac-secret": true}); err != nil {
			return nil, err
		}
	}
	if ext != nil {
		flags |= flagED
	}
	buf = append(buf, flags)
	buf = append(buf, byte(a.signCount>>24), byte(a.signCount>>16), byte(a.signCount>>8), byte(a.signCount))
	if !attested {
		return append(buf, ext...), nil
	}
	buf = append(buf, make([]byte, 16)...) // aaguid (使わない)
	buf = append(buf, byte(len(a.credID)>>8), byte(len(a.credID)))
	buf = append(buf, a.credID...)
	key := coseEC2Key{
		Kty: 2, Alg: -7, Crv: 1,
		X: a.priv.PublicKey.X.FillBytes(make([]byte, 32)), Y: a.priv.PublicKey.Y.FillBytes(make([]byte, 32)),
	}
	kb, err := cbor.Marshal(key)
	if err != nil {
		return nil, err
	}
	return append(append(buf, kb...), ext...), nil
}

// Register は、challenge (RegisterBegin が返した CreationOptions.Challenge、base64url の文字列) に対する
// AttestationResponse を組み立てる (attestation は "none")。
func (a *Authenticator) Register(rpID, origin, challenge string) (webauthn.AttestationResponse, error) {
	return a.RegisterWith(rpID, origin, challenge, Options{})
}

// RegisterWith は、Register に、Options の振る舞い (UV・PRF・拡張データ・偽装) を加える。
func (a *Authenticator) RegisterWith(rpID, origin, challenge string, o Options) (webauthn.AttestationResponse, error) {
	var resp webauthn.AttestationResponse
	authData, err := a.authData(rpID, true, o)
	if err != nil {
		return resp, err
	}
	attObj, err := cbor.Marshal(struct {
		Fmt      string         `cbor:"fmt"`
		AttStmt  map[string]any `cbor:"attStmt"`
		AuthData []byte         `cbor:"authData"`
	}{"none", map[string]any{}, authData})
	if err != nil {
		return resp, err
	}
	cdJSON, err := json.Marshal(map[string]string{"type": "webauthn.create", "challenge": challenge, "origin": origin})
	if err != nil {
		return resp, err
	}
	resp.ID = b64.EncodeToString(a.credID)
	resp.Response.ClientDataJSON = b64.EncodeToString(cdJSON)
	resp.Response.AttestationObject = b64.EncodeToString(attObj)
	resp.ClientExtensionResults = a.extensions(o, true)
	return resp, nil
}

// Authenticate は、challenge (AuthenticateBegin が返した RequestOptions.Challenge) に対する
// AssertionResponse を組み立てる (呼ぶたびに signCount を進める)。
func (a *Authenticator) Authenticate(rpID, origin, challenge string) (webauthn.AssertionResponse, error) {
	return a.AuthenticateWith(rpID, origin, challenge, Options{})
}

// AuthenticateWith は、Authenticate に、Options の振る舞いを加える。
func (a *Authenticator) AuthenticateWith(rpID, origin, challenge string, o Options) (webauthn.AssertionResponse, error) {
	var resp webauthn.AssertionResponse
	a.signCount++
	authData, err := a.authData(rpID, false, o)
	if err != nil {
		return resp, err
	}
	cdJSON, err := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": challenge, "origin": origin})
	if err != nil {
		return resp, err
	}
	cdHash := sha256.Sum256(cdJSON)
	signed := append(append([]byte{}, authData...), cdHash[:]...)
	hh := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, a.priv, hh[:])
	if err != nil {
		return resp, err
	}
	resp.ID = b64.EncodeToString(a.credID)
	resp.Response.ClientDataJSON = b64.EncodeToString(cdJSON)
	resp.Response.AuthenticatorData = b64.EncodeToString(authData)
	resp.Response.Signature = b64.EncodeToString(sig)
	resp.ClientExtensionResults = a.extensions(o, false)
	return resp, nil
}

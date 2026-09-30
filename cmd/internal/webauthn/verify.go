package webauthn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"

	"github.com/fxamacker/cbor/v2"
)

// PRFSize は、PRF の出力の大きさ (バイト)。vault.PRFSize と同じ (この package は vault を import しない)。
const PRFSize = 32

// ClientExtensionResults は、ブラウザの getClientExtensionResults() を、フロントエンドの JS が、base64url の文字列にしたもの。
// **署名の外にあり、クライアントが任意の値を送れる** (ADR 0033 決定 5)。PRF の値は、呼び手が、復号を試す入力にだけ使う。
type ClientExtensionResults struct {
	PRF *PRFResult `json:"prf,omitempty"`
}

// PRFResult は、PRF 拡張の結果。Enabled は登録時 (この passkey が PRF に対応するか)、Results は、評価した出力。
type PRFResult struct {
	Enabled bool `json:"enabled,omitempty"`
	Results *struct {
		First string `json:"first"`
	} `json:"results,omitempty"`
}

// prfFirst は、PRF の first の出力 (PRFSize バイト) を返す。無ければ (nil, nil)。あるが形が不正なら error。
func (e ClientExtensionResults) prfFirst() ([]byte, error) {
	if e.PRF == nil || e.PRF.Results == nil || e.PRF.Results.First == "" {
		return nil, nil
	}
	b, err := b64.DecodeString(e.PRF.Results.First)
	if err != nil || len(b) != PRFSize {
		return nil, fmt.Errorf("webauthn: PRF の出力が不正 (base64url の %d バイトのこと)", PRFSize)
	}
	return b, nil
}

// attested は、verifyAttestation が通した、新しい passkey。
type attested struct {
	cred       storedCredential
	prfEnabled bool
	prf        []byte // 登録時の clientExtensionResults の prf.results.first (無ければ nil)。検証できない値。
}

// verifyAttestation は、登録の応答 (attestation "none") を検証して、新しい passkey を返す。requireUV なら、UV フラグを要求する。
func verifyAttestation(cfg Config, challenge []byte, resp AttestationResponse, requireUV bool) (*attested, error) {
	clientDataJSON, err := b64.DecodeString(resp.Response.ClientDataJSON)
	if err != nil {
		return nil, fmt.Errorf("webauthn: clientDataJSON が base64url でない: %w", err)
	}
	if err := verifyClientData(clientDataJSON, "webauthn.create", challenge, cfg.Origin); err != nil {
		return nil, err
	}
	attObjBytes, err := b64.DecodeString(resp.Response.AttestationObject)
	if err != nil {
		return nil, fmt.Errorf("webauthn: attestationObject が base64url でない: %w", err)
	}
	var attObj struct {
		Fmt      string                 `cbor:"fmt"`
		AttStmt  map[string]interface{} `cbor:"attStmt"`
		AuthData []byte                 `cbor:"authData"`
	}
	if err := cbor.Unmarshal(attObjBytes, &attObj); err != nil {
		return nil, fmt.Errorf("webauthn: attestationObject を読めない: %w", err)
	}
	if attObj.Fmt != "none" {
		return nil, fmt.Errorf("webauthn: 対応していない attestation の形式 (fmt=%q、\"none\" だけに対応)", attObj.Fmt)
	}
	authData, err := parseAuthenticatorData(attObj.AuthData)
	if err != nil {
		return nil, err
	}
	if err := checkRPIDHash(authData.RPIDHash, cfg.RPID); err != nil {
		return nil, err
	}
	if !authData.UserPresent() {
		return nil, errors.New("webauthn: user present フラグが立っていない")
	}
	if requireUV && !authData.UserVerified() {
		return nil, errors.New("webauthn: user verified フラグが立っていない")
	}
	if len(authData.CredentialID) == 0 || len(authData.CredentialID) > maxCredentialIDBytes {
		return nil, fmt.Errorf("webauthn: 登録の credential ID の大きさが不正 (1〜%d バイト)", maxCredentialIDBytes)
	}
	respID, err := b64.DecodeString(resp.ID)
	if err != nil || subtle.ConstantTimeCompare(respID, authData.CredentialID) != 1 {
		return nil, errors.New("webauthn: 応答の id と、authenticatorData の credentialId が一致しない")
	}
	pub, err := parseCOSEPublicKey(authData.CredentialPublicKey)
	if err != nil {
		return nil, err
	}
	prf, err := resp.ClientExtensionResults.prfFirst()
	if err != nil {
		return nil, err
	}
	return &attested{
		cred: storedCredential{
			ID: authData.CredentialID, PublicKeyX: pub.X.Bytes(), PublicKeyY: pub.Y.Bytes(),
			SignCount: authData.SignCount, RPID: cfg.RPID,
		},
		prfEnabled: resp.ClientExtensionResults.PRF != nil && resp.ClientExtensionResults.PRF.Enabled,
		prf:        prf,
	}, nil
}

// verifyAssertion は、認証の応答を、cred の公開鍵で検証する (challenge・origin・rpIdHash・UP・署名)。requireUV なら、UV も要求する。
// 署名の対象は、authenticatorData と clientDataHash (clientExtensionResults は含まない)。通った authData を返す。
func verifyAssertion(cfg Config, cred *storedCredential, challenge []byte, resp AssertionResponse, requireUV bool) (*authenticatorData, error) {
	respID, err := b64.DecodeString(resp.ID)
	if err != nil || subtle.ConstantTimeCompare(respID, cred.ID) != 1 {
		return nil, errors.New("webauthn: 応答の id が、登録済みの credential と一致しない")
	}
	clientDataJSON, err := b64.DecodeString(resp.Response.ClientDataJSON)
	if err != nil {
		return nil, fmt.Errorf("webauthn: clientDataJSON が base64url でない: %w", err)
	}
	if err := verifyClientData(clientDataJSON, "webauthn.get", challenge, cfg.Origin); err != nil {
		return nil, err
	}
	authDataBytes, err := b64.DecodeString(resp.Response.AuthenticatorData)
	if err != nil {
		return nil, fmt.Errorf("webauthn: authenticatorData が base64url でない: %w", err)
	}
	authData, err := parseAuthenticatorData(authDataBytes)
	if err != nil {
		return nil, err
	}
	if err := checkRPIDHash(authData.RPIDHash, cfg.RPID); err != nil {
		return nil, err
	}
	if !authData.UserPresent() {
		return nil, errors.New("webauthn: user present フラグが立っていない")
	}
	if requireUV && !authData.UserVerified() {
		return nil, errors.New("webauthn: user verified フラグが立っていない")
	}
	sig, err := b64.DecodeString(resp.Response.Signature)
	if err != nil {
		return nil, fmt.Errorf("webauthn: signature が base64url でない: %w", err)
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(cred.PublicKeyX), Y: new(big.Int).SetBytes(cred.PublicKeyY)}
	clientDataHash := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte{}, authDataBytes...), clientDataHash[:]...)
	if err := verifyES256(pub, signed, sig); err != nil {
		return nil, err
	}
	return authData, nil
}

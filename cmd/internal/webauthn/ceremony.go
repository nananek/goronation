package webauthn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// challengeTTL は、発行した challenge (state token) を受け付ける期限。
const challengeTTL = 2 * time.Minute

// SessionTTL は、AuthenticateFinish が発行するセッション token の有効期限 (呼び手が、cookie の MaxAge に
// も使う値)。
const SessionTTL = 30 * 24 * time.Hour

// bootstrapUserID は、この passkey が結び付く WebAuthn の user.id (RFC の要求で opaque なバイト列が要るが、
// 1 ユーザー固定のシステムなので、区別する必要が無く、固定値でよい。個人情報は入れない)。
var bootstrapUserID = []byte("goro-admin")

// ErrNotRegistered は、まだ credential が登録されていないときの error (AuthenticateBegin)。
var ErrNotRegistered = errors.New("webauthn: まだ登録されていない")

// ErrAlreadyRegistered は、すでに credential があるときの error (RegisterBegin・RegisterFinish)。
var ErrAlreadyRegistered = errors.New("webauthn: すでに登録済み (作り直すには状態ファイルを消す)")

// ErrBadBootstrapToken は、ブートストラップトークンが無い・期限切れ・値が違うときの error。
var ErrBadBootstrapToken = errors.New("webauthn: ブートストラップトークンが無効")

// CreationOptions は、PublicKeyCredentialCreationOptions (ブラウザの navigator.credentials.create に渡す
// 形) を JSON にしたもの。challenge・user.id は base64url の文字列 (ブラウザ側の JS が ArrayBuffer に直す)。
type CreationOptions struct {
	RP                     rpEntity               `json:"rp"`
	User                   userEntity             `json:"user"`
	Challenge              string                 `json:"challenge"`
	PubKeyCredParams       []credParam            `json:"pubKeyCredParams"`
	Timeout                int                    `json:"timeout"`
	Attestation            string                 `json:"attestation"`
	AuthenticatorSelection authenticatorSelection `json:"authenticatorSelection"`
}

type rpEntity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type userEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}
type credParam struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}
type authenticatorSelection struct {
	ResidentKey      string `json:"residentKey"`
	UserVerification string `json:"userVerification"`
}

// RequestOptions は、PublicKeyCredentialRequestOptions (navigator.credentials.get に渡す形)。
type RequestOptions struct {
	RPID             string           `json:"rpId"`
	Challenge        string           `json:"challenge"`
	AllowCredentials []credDescriptor `json:"allowCredentials,omitempty"`
	Timeout          int              `json:"timeout"`
	UserVerification string           `json:"userVerification"`
}
type credDescriptor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// AttestationResponse は、ブラウザが RegisterFinish へ送る形 (PublicKeyCredential を、フロントエンドの
// JS が base64url の文字列にしたもの)。
type AttestationResponse struct {
	ID       string `json:"id"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AttestationObject string `json:"attestationObject"`
	} `json:"response"`
}

// AssertionResponse は、ブラウザが AuthenticateFinish へ送る形 (AttestationResponse と同じく、base64url
// の文字列にしたもの)。
type AssertionResponse struct {
	ID       string `json:"id"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	} `json:"response"`
}

// stateClaims は、challenge を運ぶ state token (登録・ログインの begin と finish の間) の中身。
type stateClaims struct {
	Purpose   string `json:"purpose"` // "register" か "authenticate"
	Challenge []byte `json:"challenge"`
	Expiry    int64  `json:"exp"`
}

// sessionClaims は、ログイン成功後に発行するセッション token の中身。
type sessionClaims struct {
	Purpose string `json:"purpose"` // 常に "session" (state token と取り違えないための、目的の分離)
	Expiry  int64  `json:"exp"`
}

// b64 は、base64url (パディング無し) の短縮名。
var b64 = base64.RawURLEncoding

// IssueBootstrapToken は、新しいブートストラップトークンを作り、状態に保存して (SessionSecret も無ければ
// ここで作る)、ブラウザの URL に貼り付けられる文字列で返す。goro serve token が使う。すでに登録済みでも、
// 呼べる (トークン自体は作れるが、RegisterBegin が ErrAlreadyRegistered で断る)。
func IssueBootstrapToken(ctx context.Context, st *Store, ttl time.Duration) (string, error) {
	tok, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	state, err := st.load(ctx)
	if err != nil {
		return "", err
	}
	if err := ensureSessionSecret(&state); err != nil {
		return "", err
	}
	state.BootstrapToken = tok
	state.BootstrapExpiry = time.Now().Add(ttl).Unix()
	if err := st.save(state); err != nil {
		return "", err
	}
	return b64.EncodeToString(tok), nil
}

// RegisterBegin は、bootstrapToken (IssueBootstrapToken が返した文字列) を確かめ、有効なら
// PublicKeyCredentialCreationOptions と、それに対応する state token を返す。呼び手 (HTTP ハンドラ) は、
// state をブラウザへも返し、RegisterFinish の呼び出しでそのまま受け取る (サーバー側に challenge を残さない)。
func RegisterBegin(ctx context.Context, cfg Config, st *Store, bootstrapToken string) (*CreationOptions, string, error) {
	state, err := st.load(ctx)
	if err != nil {
		return nil, "", err
	}
	if state.Credential != nil {
		return nil, "", ErrAlreadyRegistered
	}
	if err := checkBootstrapToken(state, bootstrapToken); err != nil {
		return nil, "", err
	}
	challenge, err := randomBytes(32)
	if err != nil {
		return nil, "", err
	}
	opts := &CreationOptions{
		RP:        rpEntity{ID: cfg.RPID, Name: cfg.RPName},
		User:      userEntity{ID: b64.EncodeToString(bootstrapUserID), Name: "admin", DisplayName: "goro"},
		Challenge: b64.EncodeToString(challenge),
		PubKeyCredParams: []credParam{
			{Type: "public-key", Alg: coseAlgES256},
		},
		Timeout:                int(challengeTTL / time.Millisecond),
		Attestation:            "none",
		AuthenticatorSelection: authenticatorSelection{ResidentKey: "preferred", UserVerification: "preferred"},
	}
	stateToken := signToken(state.SessionSecret, mustJSON(stateClaims{
		Purpose: "register", Challenge: challenge, Expiry: time.Now().Add(challengeTTL).Unix(),
	}))
	return opts, stateToken, nil
}

// checkBootstrapToken は、state に保存されたブートストラップトークンと、in (ブラウザから届いた文字列) を、
// 期限つき・定数時間で比べる。
func checkBootstrapToken(state persistedState, in string) error {
	if len(state.BootstrapToken) == 0 || time.Now().Unix() > state.BootstrapExpiry {
		return ErrBadBootstrapToken
	}
	got, err := b64.DecodeString(in)
	if err != nil {
		return ErrBadBootstrapToken
	}
	if len(got) != len(state.BootstrapToken) || subtle.ConstantTimeCompare(got, state.BootstrapToken) != 1 {
		return ErrBadBootstrapToken
	}
	return nil
}

// RegisterFinish は、state (RegisterBegin が返した token) と、ブラウザの attestation 応答を検証し、通れば
// credential を保存する。attestation は "none" だけを受理する (信頼チェーンは検証しない)。
func RegisterFinish(ctx context.Context, cfg Config, st *Store, state string, resp AttestationResponse) error {
	persisted, err := st.load(ctx)
	if err != nil {
		return err
	}
	if persisted.Credential != nil {
		return ErrAlreadyRegistered
	}
	claims, err := verifyStateToken(persisted.SessionSecret, state, "register")
	if err != nil {
		return err
	}
	clientDataJSON, err := b64.DecodeString(resp.Response.ClientDataJSON)
	if err != nil {
		return fmt.Errorf("webauthn: clientDataJSON が base64url でない: %w", err)
	}
	if err := verifyClientData(clientDataJSON, "webauthn.create", claims.Challenge, cfg.Origin); err != nil {
		return err
	}
	attObjBytes, err := b64.DecodeString(resp.Response.AttestationObject)
	if err != nil {
		return fmt.Errorf("webauthn: attestationObject が base64url でない: %w", err)
	}
	var attObj struct {
		Fmt      string                 `cbor:"fmt"`
		AttStmt  map[string]interface{} `cbor:"attStmt"`
		AuthData []byte                 `cbor:"authData"`
	}
	if err := cbor.Unmarshal(attObjBytes, &attObj); err != nil {
		return fmt.Errorf("webauthn: attestationObject を読めない: %w", err)
	}
	if attObj.Fmt != "none" {
		return fmt.Errorf("webauthn: 対応していない attestation の形式 (fmt=%q、\"none\" だけに対応)", attObj.Fmt)
	}
	authData, err := parseAuthenticatorData(attObj.AuthData)
	if err != nil {
		return err
	}
	if err := checkRPIDHash(authData.RPIDHash, cfg.RPID); err != nil {
		return err
	}
	if !authData.UserPresent() {
		return errors.New("webauthn: user present フラグが立っていない")
	}
	if len(authData.CredentialID) == 0 {
		return errors.New("webauthn: 登録の authenticatorData に credential が無い")
	}
	respID, err := b64.DecodeString(resp.ID)
	if err != nil || subtle.ConstantTimeCompare(respID, authData.CredentialID) != 1 {
		return errors.New("webauthn: 応答の id と、authenticatorData の credentialId が一致しない")
	}
	pub, err := parseCOSEPublicKey(authData.CredentialPublicKey)
	if err != nil {
		return err
	}
	persisted.Credential = &storedCredential{
		ID: authData.CredentialID, PublicKeyX: pub.X.Bytes(), PublicKeyY: pub.Y.Bytes(),
		SignCount: authData.SignCount, RPID: cfg.RPID,
	}
	persisted.BootstrapToken = nil
	persisted.BootstrapExpiry = 0
	return st.save(persisted)
}

// AuthenticateBegin は、登録済みの credential へのログインを求める PublicKeyCredentialRequestOptions と、
// 対応する state token を返す。
func AuthenticateBegin(ctx context.Context, cfg Config, st *Store) (*RequestOptions, string, error) {
	state, err := st.load(ctx)
	if err != nil {
		return nil, "", err
	}
	if state.Credential == nil {
		return nil, "", ErrNotRegistered
	}
	challenge, err := randomBytes(32)
	if err != nil {
		return nil, "", err
	}
	opts := &RequestOptions{
		RPID:             cfg.RPID,
		Challenge:        b64.EncodeToString(challenge),
		AllowCredentials: []credDescriptor{{Type: "public-key", ID: b64.EncodeToString(state.Credential.ID)}},
		Timeout:          int(challengeTTL / time.Millisecond),
		UserVerification: "preferred",
	}
	stateToken := signToken(state.SessionSecret, mustJSON(stateClaims{
		Purpose: "authenticate", Challenge: challenge, Expiry: time.Now().Add(challengeTTL).Unix(),
	}))
	return opts, stateToken, nil
}

// AuthenticateFinish は、state と、ブラウザの assertion 応答を検証し、通ればセッション token を返す。
func AuthenticateFinish(ctx context.Context, cfg Config, st *Store, state string, resp AssertionResponse) (string, error) {
	persisted, err := st.load(ctx)
	if err != nil {
		return "", err
	}
	if persisted.Credential == nil {
		return "", ErrNotRegistered
	}
	claims, err := verifyStateToken(persisted.SessionSecret, state, "authenticate")
	if err != nil {
		return "", err
	}
	respID, err := b64.DecodeString(resp.ID)
	if err != nil || subtle.ConstantTimeCompare(respID, persisted.Credential.ID) != 1 {
		return "", errors.New("webauthn: 応答の id が、登録済みの credential と一致しない")
	}
	clientDataJSON, err := b64.DecodeString(resp.Response.ClientDataJSON)
	if err != nil {
		return "", fmt.Errorf("webauthn: clientDataJSON が base64url でない: %w", err)
	}
	if err := verifyClientData(clientDataJSON, "webauthn.get", claims.Challenge, cfg.Origin); err != nil {
		return "", err
	}
	authDataBytes, err := b64.DecodeString(resp.Response.AuthenticatorData)
	if err != nil {
		return "", fmt.Errorf("webauthn: authenticatorData が base64url でない: %w", err)
	}
	authData, err := parseAuthenticatorData(authDataBytes)
	if err != nil {
		return "", err
	}
	if err := checkRPIDHash(authData.RPIDHash, cfg.RPID); err != nil {
		return "", err
	}
	if !authData.UserPresent() {
		return "", errors.New("webauthn: user present フラグが立っていない")
	}
	sig, err := b64.DecodeString(resp.Response.Signature)
	if err != nil {
		return "", fmt.Errorf("webauthn: signature が base64url でない: %w", err)
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(persisted.Credential.PublicKeyX),
		Y:     new(big.Int).SetBytes(persisted.Credential.PublicKeyY),
	}
	clientDataHash := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte{}, authDataBytes...), clientDataHash[:]...)
	if err := verifyES256(pub, signed, sig); err != nil {
		return "", err
	}
	// signCount のクローン検知はしない (doc.go の「限界」を参照): 増えたときだけ反映する。
	if authData.SignCount > 0 {
		persisted.Credential.SignCount = authData.SignCount
	}
	if err := st.save(persisted); err != nil {
		return "", err
	}
	return signToken(persisted.SessionSecret, mustJSON(sessionClaims{
		Purpose: "session", Expiry: time.Now().Add(SessionTTL).Unix(),
	})), nil
}

// VerifySession は、token (AuthenticateFinish が返したもの) が、有効なセッションかを確かめる。
func VerifySession(ctx context.Context, st *Store, token string) error {
	state, err := st.load(ctx)
	if err != nil {
		return err
	}
	if len(state.SessionSecret) == 0 {
		return ErrBadToken
	}
	payload, err := verifyToken(state.SessionSecret, token)
	if err != nil {
		return err
	}
	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ErrBadToken
	}
	if claims.Purpose != "session" {
		return ErrBadToken
	}
	if time.Now().Unix() > claims.Expiry {
		return errors.New("webauthn: セッションの期限が切れている")
	}
	return nil
}

// verifyStateToken は、token の署名を確かめ、purpose が一致し、期限内であることを確かめて、中身を返す。
func verifyStateToken(secret []byte, token, wantPurpose string) (stateClaims, error) {
	payload, err := verifyToken(secret, token)
	if err != nil {
		return stateClaims{}, err
	}
	var claims stateClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return stateClaims{}, ErrBadToken
	}
	if claims.Purpose != wantPurpose {
		return stateClaims{}, ErrBadToken
	}
	if time.Now().Unix() > claims.Expiry {
		return stateClaims{}, errors.New("webauthn: challenge の期限が切れている (もう一度やり直す)")
	}
	return claims, nil
}

// checkRPIDHash は、got (authenticatorData の rpIdHash) が、SHA-256(rpID) と一致するかを確かめる。
func checkRPIDHash(got [32]byte, rpID string) error {
	want := sha256.Sum256([]byte(rpID))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return errors.New("webauthn: rpIdHash が、設定した RP ID と一致しない")
	}
	return nil
}

// mustJSON は、v (この package が組み立てた、固定の構造体) を JSON にする。失敗は起こらない想定 (起きたら bug)。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("webauthn: 固定の構造体の JSON 化に失敗した: " + err.Error())
	}
	return b
}

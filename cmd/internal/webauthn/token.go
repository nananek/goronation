package webauthn

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// ErrBadToken は、signToken が作った形式の token (state・セッション) が、壊れている・署名が合わないときの error。
var ErrBadToken = errors.New("webauthn: token が壊れている、または署名が合わない")

// signToken は、payload を secret で HMAC-SHA256 署名し、"base64url(payload).base64url(mac)" にする。
// state (challenge を運ぶ) とセッション token の、どちらもこの形。
func signToken(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyToken は、token の署名を確かめ、payload を返す (期限などの中身の検査は、呼び手が行う)。
func verifyToken(secret []byte, token string) ([]byte, error) {
	p, s, ok := strings.Cut(token, ".")
	if !ok {
		return nil, ErrBadToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, ErrBadToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrBadToken
	}
	return payload, nil
}

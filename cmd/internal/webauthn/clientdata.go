package webauthn

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// clientData は、CollectedClientData (WebAuthn L2, §5.8.1) のうち、検証に使うフィールドだけ。
type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"` // base64url、パディング無し (仕様が定める形)
	Origin    string `json:"origin"`
}

// verifyClientData は、raw (clientDataJSON) を読み、type・challenge・origin が期待どおりであることを、
// 定数時間の比較 (challenge) を含めて確かめる。
func verifyClientData(raw []byte, wantType string, wantChallenge []byte, wantOrigin string) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return fmt.Errorf("webauthn: clientDataJSON を読めない: %w", err)
	}
	if cd.Type != wantType {
		return fmt.Errorf("webauthn: clientData.type = %q, want %q", cd.Type, wantType)
	}
	if cd.Origin != wantOrigin {
		return errors.New("webauthn: clientData.origin が、設定した origin と違う")
	}
	got, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	if err != nil {
		return fmt.Errorf("webauthn: clientData.challenge が base64url でない: %w", err)
	}
	if len(got) != len(wantChallenge) || subtle.ConstantTimeCompare(got, wantChallenge) != 1 {
		return errors.New("webauthn: challenge が一致しない")
	}
	return nil
}

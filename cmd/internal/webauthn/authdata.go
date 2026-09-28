package webauthn

import (
	"encoding/binary"
	"errors"
)

// authData のビット (WebAuthn L2, §6.1 Authenticator Data)。
const (
	flagUP = 1 << 0 // User Present
	flagUV = 1 << 2 // User Verified
	flagAT = 1 << 6 // Attested credential data included
	flagED = 1 << 7 // Extension data included
)

// authenticatorData は、authData (バイト列。CBOR ではない固定形式) をパースした結果。
type authenticatorData struct {
	RPIDHash            [32]byte
	Flags               byte
	SignCount           uint32
	CredentialID        []byte // AT フラグがあるとき (登録) だけ
	CredentialPublicKey []byte // 同上。COSE 鍵の生のバイト列 (まだ CBOR デコードしていない)
}

// UserPresent・UserVerified は、対応するフラグが立っているか。
func (a *authenticatorData) UserPresent() bool  { return a.Flags&flagUP != 0 }
func (a *authenticatorData) UserVerified() bool { return a.Flags&flagUV != 0 }

// parseAuthenticatorData は、b をパースする。拡張データ (ED フラグ) は対応しない (error): AT フラグが立つ
// (登録の) ときは、credentialPublicKey の CBOR が b の末尾まで、余分なバイト無く続くことを前提にする
// (cose.go の parseCOSEPublicKey が、その前提を cbor.Unmarshal の余りの検査で確かめる)。
func parseAuthenticatorData(b []byte) (*authenticatorData, error) {
	const headerLen = 32 + 1 + 4
	if len(b) < headerLen {
		return nil, errors.New("webauthn: authenticatorData が短すぎる")
	}
	var a authenticatorData
	copy(a.RPIDHash[:], b[:32])
	a.Flags = b[32]
	a.SignCount = binary.BigEndian.Uint32(b[33:37])
	rest := b[headerLen:]
	if a.Flags&flagED != 0 {
		return nil, errors.New("webauthn: 拡張データ (ED フラグ) には対応していない")
	}
	if a.Flags&flagAT == 0 {
		if len(rest) != 0 {
			return nil, errors.New("webauthn: AT フラグが無いのに、余分なバイトがある")
		}
		return &a, nil
	}
	const aaguidLen = 16
	if len(rest) < aaguidLen+2 {
		return nil, errors.New("webauthn: attestedCredentialData が短すぎる")
	}
	rest = rest[aaguidLen:] // aaguid は使わない (attestation の信頼チェーンを検証しないため)
	idLen := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	if len(rest) < idLen {
		return nil, errors.New("webauthn: credentialId が短すぎる")
	}
	a.CredentialID = rest[:idLen]
	a.CredentialPublicKey = rest[idLen:]
	if len(a.CredentialPublicKey) == 0 {
		return nil, errors.New("webauthn: credentialPublicKey が無い")
	}
	return &a, nil
}

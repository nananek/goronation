package webauthn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/fxamacker/cbor/v2"
)

// authData のビット (WebAuthn L2, §6.1 Authenticator Data)。
const (
	flagUP = 1 << 0 // User Present
	flagUV = 1 << 2 // User Verified
	flagAT = 1 << 6 // Attested credential data included
	flagED = 1 << 7 // Extension data included
)

// maxExtensionEntries は、authData の拡張データの map の項目数の上限。
const maxExtensionEntries = 4

// allowedExtensions は、authData の拡張データ (ED フラグ) に、あってよいキー (CTAP の拡張の識別子) と、その値の型の検査。
// hmac-secret は、WebAuthn の PRF 拡張の土台 (登録時は true・認証時は暗号化された出力のバイト列)。PRF の出力そのものは
// clientExtensionResults (署名の外) で届き、authData の値は使わない。credProtect は、登録時に返る (uint)。
// 実機の authData にどの拡張が載るかは、S9 (iPhone 実機の検証) で確かめる。載らない・足りないキーが見つかったら、ここに足す
// (未知のキーは、fail closed で拒否する)。
var allowedExtensions = map[string]func(v any) bool{
	"hmac-secret": func(v any) bool {
		switch x := v.(type) {
		case bool:
			return true
		case []byte:
			return len(x) <= 128
		}
		return false
	},
	"credProtect": func(v any) bool { _, ok := v.(uint64); return ok },
}

// strictDec は、authData の中の CBOR を読む設定 (入れ子・要素数・重複キーに上限を置く)。
var strictDec = func() cbor.DecMode {
	m, err := cbor.DecOptions{MaxNestedLevels: 4, MaxArrayElements: 16, MaxMapPairs: 16, DupMapKey: cbor.DupMapKeyEnforcedAPF}.DecMode()
	if err != nil {
		panic("webauthn: CBOR の設定が不正: " + err.Error())
	}
	return m
}()

// authenticatorData は、authData (バイト列。CBOR ではない固定形式) をパースした結果。
type authenticatorData struct {
	RPIDHash            [32]byte
	Flags               byte
	SignCount           uint32
	CredentialID        []byte   // AT フラグがあるとき (登録) だけ
	CredentialPublicKey []byte   // 同上。COSE 鍵の生のバイト列 (CBOR の 1 つ分だけ。まだデコードしていない)
	Extensions          []string // ED フラグがあるとき、拡張データのキー (allowedExtensions だけ)
}

// UserPresent・UserVerified は、対応するフラグが立っているか。
func (a *authenticatorData) UserPresent() bool  { return a.Flags&flagUP != 0 }
func (a *authenticatorData) UserVerified() bool { return a.Flags&flagUV != 0 }

// parseAuthenticatorData は、b をパースする。AT フラグがあれば、attestedCredentialData の後 (credentialPublicKey の CBOR
// は 1 つ分だけ)、ED フラグがあれば、その後 (AT が無ければヘッダーの直後) に、拡張データの CBOR の map が、b の末尾まで、
// 余分なバイト無く続く。ED フラグが無いのに余りがある・ED フラグがあるのに map が無い・allowedExtensions に無いキー・値の型が
// 違うものは、error (fail closed)。
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
	if a.Flags&flagAT != 0 {
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
		rest = rest[idLen:]
		var raw cbor.RawMessage
		after, err := strictDec.UnmarshalFirst(rest, &raw)
		if err != nil || len(raw) == 0 {
			return nil, errors.New("webauthn: credentialPublicKey を読めない")
		}
		a.CredentialPublicKey = rest[:len(rest)-len(after)]
		rest = after
	}
	if a.Flags&flagED == 0 {
		if len(rest) != 0 {
			return nil, errors.New("webauthn: ED フラグが無いのに、余分なバイトがある")
		}
		return &a, nil
	}
	keys, err := parseExtensions(rest)
	if err != nil {
		return nil, err
	}
	a.Extensions = keys
	return &a, nil
}

// parseExtensions は、拡張データ (CBOR の map。b の全体) を読み、キーを返す (ソート済み)。
func parseExtensions(b []byte) ([]string, error) {
	var m map[string]any
	rest, err := strictDec.UnmarshalFirst(b, &m)
	if err != nil {
		return nil, errors.New("webauthn: 拡張データ (ED フラグ) を読めない")
	}
	if len(rest) != 0 {
		return nil, errors.New("webauthn: 拡張データの後ろに、余分なバイトがある")
	}
	if len(m) == 0 || len(m) > maxExtensionEntries {
		return nil, errors.New("webauthn: 拡張データの項目の数が不正")
	}
	keys := make([]string, 0, len(m))
	for k, v := range m {
		ok, known := allowedExtensions[k]
		if !known || !ok(v) {
			return nil, fmt.Errorf("webauthn: 対応していない拡張データ (%q)", k)
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}

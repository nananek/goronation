package webauthn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/fxamacker/cbor/v2"
)

// COSE (RFC 9052・RFC 9053) の値。ES256 (P-256 ECDSA) だけを扱う。
const (
	coseKtyEC2   = 2
	coseAlgES256 = -7
	coseCrvP256  = 1
)

// coseEC2Key は、COSE_Key の EC2 (kty=2) だけを取り出す。負の整数キーは、COSE の Elliptic Curve Key の
// 共通パラメータ (RFC 9053) による。cbor の keyasint タグで、整数キーの map として読む。
type coseEC2Key struct {
	Kty int    `cbor:"1,keyasint"`
	Alg int    `cbor:"3,keyasint"`
	Crv int    `cbor:"-1,keyasint"`
	X   []byte `cbor:"-2,keyasint"`
	Y   []byte `cbor:"-3,keyasint"`
}

// parseCOSEPublicKey は、b (credentialPublicKey。CBOR の COSE_Key) を、ES256 (P-256) の公開鍵として解釈
// する。それ以外のアルゴリズム・曲線は、このバージョンでは対応しない (error)。b は、余分なバイトを含まない
// こと (cbor.Unmarshal が、末尾の余りを error にする。authenticatorData の拡張データを受け付けない設計と
// 対になる)。
func parseCOSEPublicKey(b []byte) (*ecdsa.PublicKey, error) {
	var k coseEC2Key
	if err := cbor.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("webauthn: COSE 鍵を読めない: %w", err)
	}
	if k.Kty != coseKtyEC2 {
		return nil, fmt.Errorf("webauthn: 対応していない鍵の種類 (kty=%d、EC2 だけに対応)", k.Kty)
	}
	if k.Alg != coseAlgES256 {
		return nil, fmt.Errorf("webauthn: 対応していないアルゴリズム (alg=%d、ES256 だけに対応)", k.Alg)
	}
	if k.Crv != coseCrvP256 {
		return nil, fmt.Errorf("webauthn: 対応していない曲線 (crv=%d、P-256 だけに対応)", k.Crv)
	}
	if len(k.X) != 32 || len(k.Y) != 32 {
		return nil, errors.New("webauthn: 公開鍵の座標の長さが違う (P-256 は 32 バイトずつ)")
	}
	curve := elliptic.P256()
	x := new(big.Int).SetBytes(k.X)
	y := new(big.Int).SetBytes(k.Y)
	if !curve.IsOnCurve(x, y) {
		return nil, errors.New("webauthn: 公開鍵が P-256 の曲線上にない")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// verifyES256 は、signed (authenticatorData || SHA-256(clientDataJSON)) に対する DER 署名 sig を、pub で
// 確かめる。
func verifyES256(pub *ecdsa.PublicKey, signed, sig []byte) error {
	h := sha256.Sum256(signed)
	if !ecdsa.VerifyASN1(pub, h[:], sig) {
		return errors.New("webauthn: 署名を確認できない")
	}
	return nil
}

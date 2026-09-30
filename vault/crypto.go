package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// 鍵・乱数の大きさ (ADR 0033)。
const (
	// PRFSize は、passkey の PRF 出力の大きさ (バイト)。
	PRFSize = 32
	// SaltSize は、passkey ごとの PRF の salt の大きさ (バイト)。
	SaltSize = 32

	keySize     = 32
	vaultIDSize = 16
	itemRndSize = 16
	nonceSize   = 12
)

const (
	infoWrap = "goronation/vault/wrap/v1\x00"
	infoItem = "goronation/vault/item/v1\x00"
	aadWrap  = "goronation/vault/wrap-aad/v1"
	aadItem  = "goronation/vault/item-aad/v1"
)

// errAuth は、AEAD の復号の失敗 (鍵・AAD・暗号文の、どれが違うかは区別しない)。
var errAuth = errors.New("vault: authentication failed")

// randBytes は、n バイトの乱数を返す。
func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("vault: 乱数を得られない: %w", err)
	}
	return b, nil
}

// aad は、AAD を作る: 各要素を、長さ (4 バイト) つきで連結する (要素の境界があいまいにならない)。
func aad(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = binary.BigEndian.AppendUint32(out, uint32(len(p)))
		out = append(out, p...)
	}
	return out
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// deriveWrapKey は、PRF 出力から、passkey のラップ鍵を作る (HKDF-SHA256。salt は Vault の ID、info は用途と credential ID)。
func deriveWrapKey(prf, vaultID []byte, credentialID string) ([]byte, error) {
	return hkdf.Key(sha256.New, prf, vaultID, infoWrap+credentialID, keySize)
}

// wrapAAD は、ラップの AAD (Vault の ID・credential ID・PRF の salt・形式の版)。
func wrapAAD(vaultID []byte, credentialID string, salt []byte) []byte {
	return aad([]byte(aadWrap), vaultID, []byte(credentialID), salt, []byte{formatVersion})
}

// wrapBodyKey は、本体鍵を、ラップ鍵で包む (AES-256-GCM。nonce は乱数)。返すのは nonce と暗号文。
func wrapBodyKey(bodyKey, prf, vaultID []byte, credentialID string, salt []byte) (nonce, ct []byte, err error) {
	wk, err := deriveWrapKey(prf, vaultID, credentialID)
	if err != nil {
		return nil, nil, err
	}
	defer clear(wk)
	g, err := newGCM(wk)
	if err != nil {
		return nil, nil, err
	}
	nonce, err = randBytes(nonceSize)
	if err != nil {
		return nil, nil, err
	}
	return nonce, g.Seal(nil, nonce, bodyKey, wrapAAD(vaultID, credentialID, salt)), nil
}

// unwrapBodyKey は、ラップから本体鍵を取り出す。PRF・credential ID・salt・暗号文のどれが違っても、errAuth (区別しない)。
// 返す本体鍵は、呼び手が clear する。
func unwrapBodyKey(prf, vaultID []byte, credentialID string, salt, nonce, ct []byte) ([]byte, error) {
	if len(nonce) != nonceSize {
		return nil, errAuth
	}
	wk, err := deriveWrapKey(prf, vaultID, credentialID)
	if err != nil {
		return nil, errAuth
	}
	defer clear(wk)
	g, err := newGCM(wk)
	if err != nil {
		return nil, errAuth
	}
	bk, err := g.Open(nil, nonce, ct, wrapAAD(vaultID, credentialID, salt))
	if err != nil || len(bk) != keySize {
		clear(bk)
		return nil, errAuth
	}
	return bk, nil
}

// itemKey は、本体鍵から、項目の鍵を作る。info に書き込みごとの乱数 rnd を入れるので、書き込みのたびに鍵が変わる。
func itemKey(bodyKey, vaultID []byte, kind, name string, rnd []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, bodyKey, vaultID, infoItem+kind+"\x00"+name+"\x00"+string(rnd), keySize)
}

// itemAAD は、項目の AAD (Vault の ID・種類・名前・形式の版)。
func itemAAD(vaultID []byte, kind, name string) []byte {
	return aad([]byte(aadItem), vaultID, []byte(kind), []byte(name), []byte{formatVersion})
}

// sealItem は、項目を暗号化する。書き込みごとに新しい乱数 rnd を作り、鍵が毎回変わるので、nonce は固定 (ゼロ) でよい。
func sealItem(bodyKey, vaultID []byte, kind, name string, plaintext []byte) (rnd, ct []byte, err error) {
	rnd, err = randBytes(itemRndSize)
	if err != nil {
		return nil, nil, err
	}
	k, err := itemKey(bodyKey, vaultID, kind, name, rnd)
	if err != nil {
		return nil, nil, err
	}
	defer clear(k)
	g, err := newGCM(k)
	if err != nil {
		return nil, nil, err
	}
	return rnd, g.Seal(nil, make([]byte, nonceSize), plaintext, itemAAD(vaultID, kind, name)), nil
}

// openItem は、項目を復号する。返す平文は、呼び手が使い終えたら clear する。
func openItem(bodyKey, vaultID []byte, kind, name string, rnd, ct []byte) ([]byte, error) {
	if len(rnd) != itemRndSize {
		return nil, errAuth
	}
	k, err := itemKey(bodyKey, vaultID, kind, name, rnd)
	if err != nil {
		return nil, errAuth
	}
	defer clear(k)
	g, err := newGCM(k)
	if err != nil {
		return nil, errAuth
	}
	pt, err := g.Open(nil, make([]byte, nonceSize), ct, itemAAD(vaultID, kind, name))
	if err != nil {
		return nil, errAuth
	}
	return pt, nil
}

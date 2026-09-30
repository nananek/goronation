package vault

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
)

// hkdfRef は、RFC 5869 の HKDF-SHA256 (32 バイト以内) を、crypto/hkdf を使わずに書いた参照実装。
func hkdfRef(ikm, salt []byte, info string) []byte {
	ex := hmac.New(sha256.New, salt)
	ex.Write(ikm)
	prk := ex.Sum(nil)
	ep := hmac.New(sha256.New, prk)
	ep.Write([]byte(info))
	ep.Write([]byte{1})
	return ep.Sum(nil)
}

func TestDeriveWrapKeyMatchesReference(t *testing.T) {
	prf, id := fakePRF("x"), bytes.Repeat([]byte{7}, vaultIDSize)
	got, err := deriveWrapKey(prf, id, "cred-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := hkdfRef(prf, id, "goronation/vault/wrap/v1\x00cred-1"); !bytes.Equal(got, want) {
		t.Fatalf("wrap key = %x, want %x", got, want)
	}
}

func TestItemKeyMatchesReference(t *testing.T) {
	bk, id, rnd := fakePRF("bk"), bytes.Repeat([]byte{9}, vaultIDSize), bytes.Repeat([]byte{3}, itemRndSize)
	got, err := itemKey(bk, id, "credential", "github", rnd)
	if err != nil {
		t.Fatal(err)
	}
	if want := hkdfRef(bk, id, "goronation/vault/item/v1\x00credential\x00github\x00"+string(rnd)); !bytes.Equal(got, want) {
		t.Fatalf("item key = %x, want %x", got, want)
	}
}

func TestWrapRoundTripAndBinding(t *testing.T) {
	bk, id, salt, prf := fakePRF("bk"), bytes.Repeat([]byte{1}, vaultIDSize), bytes.Repeat([]byte{2}, SaltSize), fakePRF("p")
	nonce, ct, err := wrapBodyKey(bk, prf, id, "cred-1", salt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unwrapBodyKey(prf, id, "cred-1", salt, nonce, ct)
	if err != nil || !bytes.Equal(got, bk) {
		t.Fatalf("round trip: %v", err)
	}
	other := bytes.Repeat([]byte{5}, vaultIDSize)
	flip := func(b []byte) []byte { c := bytes.Clone(b); c[0] ^= 1; return c }
	cases := map[string]func() ([]byte, error){
		"wrong prf":        func() ([]byte, error) { return unwrapBodyKey(fakePRF("q"), id, "cred-1", salt, nonce, ct) },
		"wrong vault id":   func() ([]byte, error) { return unwrapBodyKey(prf, other, "cred-1", salt, nonce, ct) },
		"wrong credential": func() ([]byte, error) { return unwrapBodyKey(prf, id, "cred-2", salt, nonce, ct) },
		"wrong salt":       func() ([]byte, error) { return unwrapBodyKey(prf, id, "cred-1", flip(salt), nonce, ct) },
		"tampered ct":      func() ([]byte, error) { return unwrapBodyKey(prf, id, "cred-1", salt, nonce, flip(ct)) },
		"tampered nonce":   func() ([]byte, error) { return unwrapBodyKey(prf, id, "cred-1", salt, flip(nonce), ct) },
		"short nonce":      func() ([]byte, error) { return unwrapBodyKey(prf, id, "cred-1", salt, nonce[:5], ct) },
		"empty ct":         func() ([]byte, error) { return unwrapBodyKey(prf, id, "cred-1", salt, nonce, nil) },
	}
	for name, f := range cases {
		if got, err := f(); err != errAuth || got != nil {
			t.Errorf("%s: (%x, %v), want errAuth", name, got, err)
		}
	}
}

func TestWrapNonceIsRandom(t *testing.T) {
	bk, id, salt, prf := fakePRF("bk"), bytes.Repeat([]byte{1}, vaultIDSize), bytes.Repeat([]byte{2}, SaltSize), fakePRF("p")
	n1, c1, _ := wrapBodyKey(bk, prf, id, "c", salt)
	n2, c2, _ := wrapBodyKey(bk, prf, id, "c", salt)
	if bytes.Equal(n1, n2) || bytes.Equal(c1, c2) {
		t.Fatal("同じ入力でも、nonce (と暗号文) は毎回変わる")
	}
}

func TestItemRoundTripBindingAndFreshKey(t *testing.T) {
	bk, id := fakePRF("bk"), bytes.Repeat([]byte{1}, vaultIDSize)
	rnd, ct, err := sealItem(bk, id, kindCredential, "github", []byte("tok"))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := openItem(bk, id, kindCredential, "github", rnd, ct); err != nil || string(pt) != "tok" {
		t.Fatalf("round trip: %q %v", pt, err)
	}
	other := bytes.Repeat([]byte{2}, vaultIDSize)
	bad := map[string]func() ([]byte, error){
		"kind":     func() ([]byte, error) { return openItem(bk, id, kindLoginState, "github", rnd, ct) },
		"name":     func() ([]byte, error) { return openItem(bk, id, kindCredential, "gitlab", rnd, ct) },
		"vault id": func() ([]byte, error) { return openItem(bk, other, kindCredential, "github", rnd, ct) },
		"body key": func() ([]byte, error) { return openItem(fakePRF("other"), id, kindCredential, "github", rnd, ct) },
		"rand": func() ([]byte, error) {
			r := bytes.Clone(rnd)
			r[0] ^= 1
			return openItem(bk, id, kindCredential, "github", r, ct)
		},
		"short rnd": func() ([]byte, error) { return openItem(bk, id, kindCredential, "github", rnd[:3], ct) },
		"ct": func() ([]byte, error) {
			c := bytes.Clone(ct)
			c[0] ^= 1
			return openItem(bk, id, kindCredential, "github", rnd, c)
		},
	}
	for name, f := range bad {
		if pt, err := f(); err != errAuth || pt != nil {
			t.Errorf("%s: (%q, %v), want errAuth", name, pt, err)
		}
	}
	rnd2, ct2, _ := sealItem(bk, id, kindCredential, "github", []byte("tok"))
	if bytes.Equal(rnd, rnd2) || bytes.Equal(ct, ct2) {
		t.Fatal("書き込みごとに、乱数と鍵 (暗号文) が変わる")
	}
}

func TestAADBoundaries(t *testing.T) {
	if bytes.Equal(aad([]byte("ab"), []byte("c")), aad([]byte("a"), []byte("bc"))) {
		t.Fatal("AAD の要素の境界があいまい")
	}
}

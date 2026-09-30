package vault

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakePRF は、テスト用の偽の PRF 出力 (seed から決まる 32 バイト)。実機の PRF は、S9 の範囲。
func fakePRF(seed string) []byte {
	h := sha256.Sum256([]byte("fake-prf:" + seed))
	return h[:]
}

func fakeSalt(t *testing.T) []byte {
	t.Helper()
	s, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newDir は、Open が作る、0700 の Vault のディレクトリの path (まだ無い)。
func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "vault")
}

// initVault は、credential "cred-a" (PRF は fakePRF("a")) を最初のラップにした、解錠中の Vault を返す。
func initVault(t *testing.T) (*Vault, string) {
	t.Helper()
	dir := newDir(t)
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	if err := v.Init("cred-a", fakeSalt(t), fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	return v, dir
}

// addPasskey は、authorizer (cred-a) の証明で、credential id を追加する (PRF は fakePRF(seed))。
func addPasskey(t *testing.T, v *Vault, id, seed string) {
	t.Helper()
	err := v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, Enrollment{CredentialID: id, Salt: fakeSalt(t), PRF: fakePRF(seed)})
	if err != nil {
		t.Fatalf("AddWrap(%s) = %v", id, err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// snapshot は、vault.json の中身。「保存内容が変わらない」の比較に使う。
func snapshot(t *testing.T, dir string) []byte { return readFile(t, filepath.Join(dir, fileName)) }

func mustEqual(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s が変わった", what)
	}
}

func fixedNow() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }

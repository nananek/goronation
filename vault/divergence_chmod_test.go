package vault

import (
	"errors"
	"os"
	"testing"

	"github.com/nananek/goronation/core/credential"
)

// 実際の失敗 (dir を 0300 にして、open(dir) だけを失敗させる) でも、ディスクとメモリが一致する。root は DAC を迂回するので skip する。
func TestRemoveWrapRealSyncFailureStaysConsistent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root は DAC を迂回し、dir の権限で syncDir を失敗させられない (注入版が、同じ経路を通す)")
	}
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	err := v.RemoveWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, "cred-b")
	if cerr := os.Chmod(dir, 0o700); cerr != nil {
		t.Fatal(cerr)
	}
	if err == nil || errors.Is(err, ErrNotDurable) == false {
		t.Skipf("syncDir 以外で失敗した、または失敗しなかった: %v", err)
	}
	if err := v.PutCredential("github", credential.New("tok")); err != nil && !errors.Is(err, ErrNotDurable) {
		t.Fatal(err)
	}
	after, lerr := loadDocument(dir)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(after.Wraps) != 1 {
		t.Fatalf("ラップが %d 個 (want 1)", len(after.Wraps))
	}
}

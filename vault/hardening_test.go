package vault

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/core/credential"
)

// lock ファイルを rename・差し替えしても、生きた持ち主が居る dir は、ErrInUse のまま (flock は dir 自身に掛かる)。
// 別インスタンスが、古い document を書き戻して、削除したラップを復活させる経路 (L-2) を、作れない。
func TestLockFileSwapCannotCreateSecondOwner(t *testing.T) {
	v, dir := initVault(t)
	lock := filepath.Join(dir, "lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(lock, lock+".old"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrInUse) {
		t.Fatalf("lock を差し替えた後の Open = %v, want ErrInUse", err)
	}
	if _, err := Reset(dir, ResetOptions{}); !errors.Is(err, ErrInUse) {
		t.Fatalf("lock を差し替えた後の Reset = %v, want ErrInUse", err)
	}
	if !v.Initialized() {
		t.Fatal("持ち主の Vault が壊れた")
	}
}

// audit.log が FIFO でも、止まらずに error (fail closed)。vault.json は変わらない。
func TestAuditFIFOFailsClosedWithoutHanging(t *testing.T) {
	v, dir := initVault(t)
	before := snapshot(t, dir)
	if err := os.Remove(filepath.Join(dir, auditName)); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, auditName), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, Enrollment{CredentialID: "cred-b", Salt: fakeSalt(t), PRF: fakePRF("b")})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO の audit.log で、AddWrap が成功した")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("audit.log が FIFO で、AddWrap が止まった")
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
}

// 緩い権限の audit.log は使わない (fail closed)。
func TestAuditWidePermissionsFailsClosed(t *testing.T) {
	v, dir := initVault(t)
	if err := os.Chmod(filepath.Join(dir, auditName), 0o644); err != nil {
		t.Fatal(err)
	}
	err := v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, Enrollment{CredentialID: "cred-b", Salt: fakeSalt(t), PRF: fakePRF("b")})
	if err == nil {
		t.Fatal("権限の広い audit.log で、AddWrap が成功した")
	}
	if len(v.WrapInfos()) != 1 {
		t.Fatal("ラップが増えた")
	}
}

// Reset は、確認した ID と、lock を取った後の ID が違えば、何も壊さない (N-1)。
func TestResetRefusesWhenVaultChangedAfterConfirmation(t *testing.T) {
	v, dir := initVault(t)
	if err := v.PutCredential("github", credential.New("tok")); err != nil {
		t.Fatal(err)
	}
	v.Close()
	before := snapshot(t, dir)
	if _, err := Reset(dir, ResetOptions{ExpectID: "00000000000000000000000000000000"}); !errors.Is(err, ErrVaultChanged) {
		t.Fatalf("Reset = %v, want ErrVaultChanged", err)
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
	id, err := PeekID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Reset(dir, ResetOptions{ExpectID: id}); err != nil || got != id {
		t.Fatalf("一致する ID の Reset = %q, %v", got, err)
	}
}

func TestWrapInfosAfterCloseIsEmpty(t *testing.T) {
	v, _ := initVault(t)
	v.Close()
	if got := v.WrapInfos(); got != nil {
		t.Fatalf("Close 後の WrapInfos = %v", got)
	}
}

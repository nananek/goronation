package vault

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/credential"
)

// failSyncDir は、ディレクトリの fsync を、失敗させる (rename の後の失敗の注入)。root でも動く。
func failSyncDir(t *testing.T) {
	t.Helper()
	orig := syncDirFn
	syncDirFn = func(string) error { return errors.New("injected EIO") }
	t.Cleanup(func() { syncDirFn = orig })
}

// rename の後の fsync が失敗しても、メモリはディスクと一致する。error を見て別の Put をしても、削除したラップは復活しない。
func TestNotDurableWriteKeepsMemoryAndDiskConsistent(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	failSyncDir(t)

	err := v.RemoveWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, "cred-b")
	if !errors.Is(err, ErrNotDurable) {
		t.Fatalf("RemoveWrap = %v, want ErrNotDurable", err)
	}
	onDisk, lerr := loadDocument(dir)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got, want := len(onDisk.Wraps), len(v.WrapInfos()); got != 1 || want != 1 {
		t.Fatalf("ディスクのラップ %d 個・メモリのラップ %d 個、want 1・1", got, want)
	}
	if err := v.PutCredential("github", credential.New("tok")); !errors.Is(err, ErrNotDurable) {
		t.Fatalf("PutCredential = %v, want ErrNotDurable (適用済み)", err)
	}
	after, lerr := loadDocument(dir)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(after.Wraps) != 1 {
		t.Fatalf("無関係な Put が、ラップの集合を巻き戻した (1 個 → %d 個)", len(after.Wraps))
	}
	v.Lock()
	if err := v.Unlock("cred-b", fakePRF("b")); !errors.Is(err, ErrUnlockFailed) {
		t.Fatalf("削除した passkey で Unlock できた: %v", err)
	}
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	if s, err := v.Token(nil, "github"); err != nil || s.Reveal() != "tok" {
		t.Fatalf("適用済みの項目を読めない: %v", err)
	}
	log, _ := os.ReadFile(dir + "/" + auditName)
	if !strings.Contains(string(log), `"remove-wrap-not-durable"`) || strings.Contains(string(log), `"remove-wrap-failed"`) {
		t.Fatalf("監査が、適用済みの削除を「失敗」と記録している:\n%s", log)
	}
}

// AddWrap も、適用済みなら、メモリに反映する。
func TestNotDurableAddWrapIsApplied(t *testing.T) {
	v, dir := initVault(t)
	failSyncDir(t)
	err := v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, Enrollment{CredentialID: "cred-b", Salt: fakeSalt(t), PRF: fakePRF("b")})
	if !errors.Is(err, ErrNotDurable) {
		t.Fatalf("AddWrap = %v, want ErrNotDurable", err)
	}
	onDisk, _ := loadDocument(dir)
	if len(onDisk.Wraps) != 2 || len(v.WrapInfos()) != 2 {
		t.Fatalf("ディスク %d 個・メモリ %d 個、want 2・2", len(onDisk.Wraps), len(v.WrapInfos()))
	}
}

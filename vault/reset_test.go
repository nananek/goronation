package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/credential"
)

func TestResetDestroysVaultAndKeepsAudit(t *testing.T) {
	v, dir := initVault(t)
	if err := v.PutCredential("github", credential.New("SECRET-TOKEN")); err != nil {
		t.Fatal(err)
	}
	id := v.doc.ID
	v.Close()

	got, err := Reset(dir, ResetOptions{Now: fixedNow})
	if err != nil {
		t.Fatalf("Reset = %v", err)
	}
	if len(got) != 2*vaultIDSize {
		t.Fatalf("消した Vault の ID = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vault.json が残っている: %v", err)
	}
	audit := string(readFile(t, filepath.Join(dir, auditName)))
	if !strings.Contains(audit, `"event":"reset"`) || !strings.Contains(audit, got) || !strings.Contains(audit, "2026-09-30T12:00:00Z") {
		t.Fatalf("監査の記録 = %q", audit)
	}
	for _, secret := range []string{"SECRET-TOKEN", string(id)} {
		if strings.Contains(audit, secret) {
			t.Fatal("監査の記録に、秘密を書かない")
		}
	}

	v2, err := Open(dir)
	if err != nil {
		t.Fatalf("Reset 後の Open = %v", err)
	}
	defer v2.Close()
	if v2.Initialized() {
		t.Fatal("Reset の後は、未初期化")
	}
	if err := v2.Unlock("cred-a", fakePRF("a")); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Reset 後の Unlock = %v", err)
	}
	// 新しい ID・新しい本体鍵で、作り直せる。古いラップは、戻らない。
	if err := v2.Init("cred-a", fakeSalt(t), fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(v2.doc.ID, id) {
		t.Fatal("Reset の後の Vault は、新しい ID")
	}
	if _, err := v2.Token(context.Background(), "github"); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("Reset の前の資格情報が残っている: %v", err)
	}
}

func TestResetWithoutOpenAndWhileLocked(t *testing.T) {
	v, dir := initVault(t)
	v.Close() // 施錠して閉じた後 (解錠していない状態) でも、reset できる
	if _, err := Reset(dir, ResetOptions{}); err != nil {
		t.Fatalf("Reset = %v", err)
	}
}

func TestResetRefusesWhileInUse(t *testing.T) {
	v, dir := initVault(t)
	before := snapshot(t, dir)
	if _, err := Reset(dir, ResetOptions{}); !errors.Is(err, ErrInUse) {
		t.Fatalf("開いている Vault の Reset = %v, want ErrInUse", err)
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
	if !v.Unlocked() {
		t.Fatal("失敗した Reset は、解錠状態を変えない")
	}
	if _, err := os.Stat(filepath.Join(dir, auditName)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(readFile(t, filepath.Join(dir, auditName))), `"event":"reset"`) {
		t.Fatal("失敗した Reset は、reset の記録を書かない")
	}
}

func TestResetDestroysBrokenVault(t *testing.T) {
	for name, content := range map[string][]byte{
		"garbage":   []byte("not json"),
		"empty":     {},
		"bad ver":   []byte(`{"version":99,"id":"AAAAAAAAAAAAAAAAAAAAAA=="}`),
		"truncated": []byte(`{"version":1,"id":"AAAAAAAAAAAAAAAAAAAAAA==","wraps":[`),
	} {
		t.Run(name, func(t *testing.T) {
			dir := newDir(t)
			v, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			v.Close()
			if err := os.WriteFile(filepath.Join(dir, fileName), content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); !errors.Is(err, ErrFormat) {
				t.Fatalf("壊れた vault.json の Open = %v, want ErrFormat", err)
			}
			id, err := Reset(dir, ResetOptions{})
			if err != nil {
				t.Fatalf("壊れた Vault の Reset = %v", err)
			}
			if name == "bad ver" && id != "00000000000000000000000000000000" {
				t.Fatalf("読めた ID = %q", id)
			}
			if name != "bad ver" && id != "unknown" {
				t.Fatalf("読めない ID = %q, want unknown", id)
			}
			if _, err := Open(dir); err != nil {
				t.Fatalf("Reset の後の Open = %v", err)
			}
		})
	}
}

func TestResetWorksWithWideFilePermissions(t *testing.T) {
	v, dir := initVault(t)
	v.Close()
	if err := os.Chmod(filepath.Join(dir, fileName), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := Reset(dir, ResetOptions{})
	if err != nil || id != "unknown" {
		t.Fatalf("Reset = %q, %v (権限が広い vault.json は、読まずに消す)", id, err)
	}
}

func TestResetMissingDirAndRealDirOnly(t *testing.T) {
	if _, err := Reset(newDir(t), ResetOptions{}); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("無い dir の Reset = %v", err)
	}
	real := newDir(t)
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(real), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Reset(link, ResetOptions{}); err == nil {
		t.Fatal("symlink の dir は、使わない")
	}
}

// 監査の記録が書けなければ、壊さない (fail closed)。
func TestResetRefusesWhenAuditCannotBeWritten(t *testing.T) {
	v, dir := initVault(t)
	v.Close()
	if err := os.Remove(filepath.Join(dir, auditName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, auditName), 0o700); err != nil { // audit.log を、ディレクトリにして、書けなくする
		t.Fatal(err)
	}
	if _, err := Reset(dir, ResetOptions{}); err == nil {
		t.Fatal("監査の記録を書けないのに、reset した")
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("壊してはいけない: %v", err)
	}
}

func TestResetAuditIsAppendOnly(t *testing.T) {
	v, dir := initVault(t)
	v.Close()
	if _, err := Reset(dir, ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	v2, _ := Open(dir)
	if err := v2.Init("cred-a", fakeSalt(t), fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	v2.Close()
	if _, err := Reset(dir, ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(dir, auditName)))), "\n")
	var events []string
	for _, l := range lines {
		for _, e := range []string{"init", "reset"} {
			if strings.Contains(l, `"event":"`+e+`"`) {
				events = append(events, e)
			}
		}
	}
	if strings.Join(events, ",") != "init,reset,init,reset" {
		t.Fatalf("記録の順 = %v (reset は、過去の記録を消さない)", events)
	}
}

package vault

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/credential"
)

func readAudit(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, auditName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("記録の 1 行が JSON でない: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

func TestAuditRecordsWrapOperations(t *testing.T) {
	dir := newDir(t)
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	v.now = fixedNow
	long := strings.Repeat("A", 40) // credential ID は、先頭だけを記録する
	if err := v.Init(long, fakeSalt(t), fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	a := Proof{CredentialID: long, PRF: fakePRF("a")}
	if err := v.AddWrap(a, Enrollment{CredentialID: "cred-b", Salt: fakeSalt(t), PRF: fakePRF("b")}); err != nil {
		t.Fatal(err)
	}
	if err := v.AddWrap(Proof{CredentialID: long, PRF: fakePRF("x")}, Enrollment{CredentialID: "cred-c", Salt: fakeSalt(t), PRF: fakePRF("c")}); !errors.Is(err, ErrUnlockFailed) {
		t.Fatal(err)
	}
	if err := v.RemoveWrap(Proof{CredentialID: "cred-b", PRF: fakePRF("b")}, long); err != nil {
		t.Fatal(err)
	}
	if err := v.RemoveWrap(Proof{CredentialID: long, PRF: fakePRF("a")}, "cred-b"); !errors.Is(err, ErrUnlockFailed) {
		t.Fatalf("削除済みの passkey を authorizer にした削除 = %v", err) // long は消えたので、証明できない
	}
	got := readAudit(t, dir)
	var events []string
	for _, e := range got {
		events = append(events, e["event"].(string))
		if e["time"] != "2026-09-30T12:00:00Z" {
			t.Errorf("time = %v", e["time"])
		}
		if int(e["uid"].(float64)) != os.Geteuid() {
			t.Errorf("uid = %v", e["uid"])
		}
		for _, k := range []string{"credential", "target"} {
			if s, ok := e[k].(string); ok && len(s) > idHead {
				t.Errorf("%s = %q は、先頭 %d 文字だけ", k, s, idHead)
			}
		}
	}
	want := "init,add-wrap,add-wrap-denied,remove-wrap,remove-wrap-denied"
	if strings.Join(events, ",") != want {
		t.Fatalf("events = %v, want %s", events, want)
	}
}

// 記録にも、vault.json の外にも、PRF・本体鍵・値を、書かない。
func TestAuditNeverContainsSecrets(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	if err := v.PutCredential("github", credential.New("TOPSECRET-TOKEN")); err != nil {
		t.Fatal(err)
	}
	_ = v.RemoveWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, "cred-b")
	v.Lock()
	_ = v.Unlock("cred-a", fakePRF("bad"))
	audit := readFile(t, filepath.Join(dir, auditName))
	disk := snapshot(t, dir)
	secrets := map[string][]byte{"PRF a": fakePRF("a"), "PRF b": fakePRF("b"), "本体鍵": nil}
	v.Unlock("cred-a", fakePRF("a"))
	secrets["本体鍵"] = bytes.Clone(v.key)
	for name, s := range secrets {
		for _, enc := range []string{string(s), hex.EncodeToString(s), base64.StdEncoding.EncodeToString(s), base64.URLEncoding.EncodeToString(s), base64.RawURLEncoding.EncodeToString(s)} {
			if strings.Contains(string(audit), enc) {
				t.Errorf("監査の記録に %s がある", name)
			}
			if name != "PRF b" && name != "PRF a" && strings.Contains(string(disk), enc) {
				t.Errorf("vault.json に %s が平文である", name)
			}
			if (name == "PRF a" || name == "PRF b") && strings.Contains(string(disk), enc) {
				t.Errorf("vault.json に %s が平文である", name)
			}
		}
	}
	if bytes.Contains(disk, []byte("TOPSECRET-TOKEN")) || bytes.Contains(audit, []byte("TOPSECRET-TOKEN")) {
		t.Fatal("資格情報の値が平文で残っている")
	}
}

// 監査の記録を書けなければ、init・追加・削除は進まない (fail closed)。
func TestWrapOperationsRefuseWithoutAudit(t *testing.T) {
	dir := newDir(t)
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := os.Mkdir(filepath.Join(dir, auditName), 0o700); err != nil { // audit.log をディレクトリにして、書けなくする
		t.Fatal(err)
	}
	if err := v.Init("cred-a", fakeSalt(t), fakePRF("a")); err == nil {
		t.Fatal("記録を書けないのに、Init できた")
	}
	if v.Initialized() {
		t.Fatal("初期化されてはいけない")
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vault.json ができた: %v", err)
	}

	// 記録が書ける Vault で初期化し、その後、書けなくする。
	if err := os.Remove(filepath.Join(dir, auditName)); err != nil {
		t.Fatal(err)
	}
	if err := v.Init("cred-a", fakeSalt(t), fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	addPasskey(t, v, "cred-b", "b")
	before := snapshot(t, dir)
	if err := os.Remove(filepath.Join(dir, auditName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, auditName), 0o700); err != nil {
		t.Fatal(err)
	}
	a := Proof{CredentialID: "cred-a", PRF: fakePRF("a")}
	if err := v.AddWrap(a, Enrollment{CredentialID: "cred-c", Salt: fakeSalt(t), PRF: fakePRF("c")}); err == nil {
		t.Fatal("記録を書けないのに、追加できた")
	}
	if err := v.RemoveWrap(Proof{CredentialID: "cred-b", PRF: fakePRF("b")}, "cred-a"); err == nil {
		t.Fatal("記録を書けないのに、削除できた")
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
	if len(v.WrapInfos()) != 2 {
		t.Fatal("メモリの状態が変わった")
	}
}

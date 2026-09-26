package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchDoesNotFollowTags は、Fetch を実行しても、refs/heads/goro/<id>/ の外に ref ができないことを確認する。
// 檻が付けたタグは、名前を檻が決めたもので (ブランチの名前の検査を通らない)、git fetch の自動追従で、利用者の repo の refs/tags/ に入る。
func TestFetchDoesNotFollowTags(t *testing.T) {
	st, _, sess := exportFixture(t)
	cageSh(t, st, sess, `set -e
echo x > cage.txt
git add cage.txt
git commit -q -m c
git tag v9.9.9
git tag 'evil;$(touch$IFS/tmp/pwn)'
`)
	b, err := st.Export(ctxT(t), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(t.TempDir(), "user")
	if err := os.Mkdir(user, 0o755); err != nil {
		t.Fatal(err)
	}
	hostGit(t, user, "init", "-q", "-b", "main", ".")
	cmd := exec.Command("/bin/sh", "-c", b.Fetch)
	cmd.Dir = user
	cmd.Env = hostEnv(t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("取り込みのコマンドが失敗した: %v\n%s\n%s", err, b.Fetch, out)
	}
	prefix := "refs/heads/goro/" + sess.ID + "/"
	for _, ref := range strings.Fields(hostGit(t, user, "for-each-ref", "--format=%(refname)")) {
		if !strings.HasPrefix(ref, prefix) {
			t.Errorf("goro/<id>/ の外に ref ができた: %s", ref)
		}
	}
}

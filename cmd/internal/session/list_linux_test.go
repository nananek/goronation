package session

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestListSessions は、List が、ID の形のディレクトリだけを、古い順に返し、作成時刻を ID から、元の repo の名前を記録から読むことを確認する。
func TestListSessions(t *testing.T) {
	st, _, _ := testStore(t)
	mk := func(id, repo string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(st.root, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if repo != "" {
			if err := os.WriteFile(filepath.Join(st.root, id, repoFile), []byte(repo), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("20260926-120000-bbbbbb", "second\n")
	mk("20260101-000000-aaaaaa", "first\n")
	mk("20260926-120000-cccccc", "") // 記録が無い
	mk("20260926-130000-dddddd", "evil\x1b[31m\u202enm\nsecond line\n")
	mk("20260926-140000-eeeeee", strings.Repeat("あ", 90)+"\n")  // 270 バイト。書く上限 (200) を超える
	mk("20260926-141000-eeeeef", strings.Repeat("あ", 300)+"\n") // 読む上限 (800 バイト) を超える
	// 数えないもの: 形が違う名前・ファイル・symlink・存在しない日時。
	mk("not-an-id", "")
	mk("20260926-120000-BBBBBB", "")
	mk("20261399-256161-abcdef", "") // 形は合うが、日時として不正
	if err := os.WriteFile(filepath.Join(st.root, "20260926-150000-ffffff"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("20260101-000000-aaaaaa", filepath.Join(st.root, "20260926-150000-999999")); err != nil {
		t.Fatal(err)
	}
	// 記録が FIFO・symlink でも、止まらず、読まない。
	mk("20260926-160000-111111", "")
	if err := syscall.Mkfifo(filepath.Join(st.root, "20260926-160000-111111", repoFile), 0o600); err != nil {
		t.Fatal(err)
	}
	mk("20260926-170000-222222", "")
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(st.root, "20260926-170000-222222", repoFile)); err != nil {
		t.Fatal(err)
	}

	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	repos := map[string]string{}
	for _, in := range got {
		ids = append(ids, in.ID)
		repos[in.ID] = in.Repo
	}
	wantIDs := []string{
		"20260101-000000-aaaaaa", "20260926-120000-bbbbbb", "20260926-120000-cccccc", "20260926-130000-dddddd",
		"20260926-140000-eeeeee", "20260926-141000-eeeeef", "20260926-160000-111111", "20260926-170000-222222",
	}
	if strings.Join(ids, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("ID の一覧 =\n%v\nwant\n%v", ids, wantIDs)
	}
	if got[0].Created != time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("Created = %v", got[0].Created)
	}
	for id, want := range map[string]string{
		"20260101-000000-aaaaaa": "first",
		"20260926-120000-bbbbbb": "second",
		"20260926-120000-cccccc": "",
		"20260926-130000-dddddd": "evil?[31m?nm", // 制御文字と双方向制御は ? にし、1 行目だけ
		"20260926-141000-eeeeef": "",             // 読む上限を超えたものは、読まない
		"20260926-160000-111111": "",
		"20260926-170000-222222": "",
	} {
		if repos[id] != want {
			t.Errorf("%s の Repo = %q, want %q", id, repos[id], want)
		}
	}
	if r := repos["20260926-140000-eeeeee"]; len(r) > maxRepoLabel || !strings.HasPrefix(r, "あ") || strings.ContainsRune(r, '�') {
		t.Errorf("長い名前が、上限で切られていない (%d バイト): %q", len(r), r)
	}
}

// TestListEmptyAndMissing は、セッションが無いとき空を返し、状態ディレクトリが消えていれば error にすることを確認する。
func TestListEmptyAndMissing(t *testing.T) {
	st, _, _ := testStore(t)
	got, err := st.List()
	if err != nil || len(got) != 0 {
		t.Fatalf("List() = %v, %v (want 空・error なし)", got, err)
	}
	if err := os.Remove(st.root); err != nil {
		t.Fatal(err)
	}
	if _, err := st.List(); err == nil {
		t.Error("<state>/sessions が無いのに、error にならない")
	}
}

// TestCreateRecordsRepoLabel は、Create が、元の repo のディレクトリ名を記録し、List に出ることを確認する。
func TestCreateRecordsRepoLabel(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	st, _, _ := testStore(t)
	sess, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil || len(got) != 1 {
		t.Fatalf("List() = %v, %v", got, err)
	}
	if got[0].ID != sess.ID || got[0].Repo != filepath.Base(src.Dir) {
		t.Errorf("List()[0] = %+v, want ID %s・Repo %s", got[0], sess.ID, filepath.Base(src.Dir))
	}
}

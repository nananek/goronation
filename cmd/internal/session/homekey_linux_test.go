package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// RepoKey は、実 path の sha256 の先頭 8 バイトの 16 進 16 桁。同じ path は同じキー、別の path は別のキー (末尾の違い・大文字小文字も別)。
func TestRepoKey(t *testing.T) {
	// sha256 の先頭 8 バイトの 16 進 (python の hashlib で、実装とは別に計算した値)。
	for path, want := range map[string]string{"/home/u/work/foo": "1dea698187063237", "/a": "6a50dc8584134c7d"} {
		if got := RepoKey(path); got != want {
			t.Errorf("RepoKey(%q) = %q, want %q", path, got, want)
		}
	}
	seen := map[string]string{}
	for _, p := range []string{"/a", "/a/", "/A", "/a/b", "/b", "/home/u/work/foo", "/home/u/work/foo2", "/home/u/work/fo"} {
		k := RepoKey(p)
		if !homeKeyRE.MatchString(k) {
			t.Errorf("RepoKey(%q) = %q が、形に合わない", p, k)
		}
		if prev, dup := seen[k]; dup {
			t.Errorf("RepoKey(%q) が、%q と同じ: %s", p, prev, k)
		}
		seen[k] = p
		if RepoKey(p) != k {
			t.Errorf("RepoKey(%q) が、呼ぶたびに違う", p)
		}
	}
}

// Store.HomeKey は、記録が無ければ空 (error ではない: repo ごとに分ける前に作ったセッション)、正しい記録ならそのキーを返す。
// 壊れた記録 (形・大きさ・種類が違う) は、error: 黙って別の repo の HOME を選ばない。中身は、error に出さない・読まない。
func TestStoreHomeKey(t *testing.T) {
	st, _, _ := testStore(t)

	sess := mkSession(t, st, "20260926-120000-aaaaaa")
	if got, err := st.HomeKey(sess); got != "" || err != nil {
		t.Errorf("記録が無い: HomeKey = %q, %v (want 空・error なし)", got, err)
	}
	for i, content := range []string{"0123456789abcdef\n", "0123456789abcdef", "ffffffffffffffff\n"} {
		s := mkSession(t, st, fmt.Sprintf("20260926-13%04d-bbbbbb", i))
		if err := os.WriteFile(filepath.Join(s.Dir, homeKeyFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := st.HomeKey(s); got != strings.TrimSpace(content) || err != nil {
			t.Errorf("記録 %q: HomeKey = %q, %v", content, got, err)
		}
	}

	// 壊れた記録: 桁数が違う・大文字・16 進でない・path 要素になりうる文字・複数行・空・大きすぎる・制御文字。
	bad := []string{"0123456789abcde\n", "0123456789abcdef0\n", "0123456789ABCDEF\n", "0123456789abcdeg\n", "../../../etc/xxxx\n", "0123456789abcdef\n\n",
		"0123456789abcdef\nx\n", "\n", "", " 0123456789abcdef\n", "0123456789abcdef \n", "0123456789abcdef\r\n", "01234567\x00abcdef\n", "0123456789abc\x1b[31m\n",
		strings.Repeat("a", 100) + "\n", "claude\n"}
	for i, content := range bad {
		s := mkSession(t, st, fmt.Sprintf("20260926-14%04d-cccccc", i))
		if err := os.WriteFile(filepath.Join(s.Dir, homeKeyFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := st.HomeKey(s); err == nil || got != "" {
			t.Errorf("壊れた記録 %q: HomeKey = %q, %v (want error)", content, got, err)
		} else if strings.ContainsAny(err.Error(), "\x1b\x00") {
			t.Errorf("error に、記録の制御文字がそのまま出ている: %q", err)
		}
	}

	// 通常のファイルでない記録 (FIFO・symlink・ディレクトリ): 止まらず、読まず、error。symlink の先の中身は、読まれない。
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("0123456789abcdef\n"), 0o600); err != nil { // 形は正しい中身。読まれたら、そのキーになってしまう
		t.Fatal(err)
	}
	fifo := mkSession(t, st, "20260926-150000-dddddd")
	if err := syscall.Mkfifo(filepath.Join(fifo.Dir, homeKeyFile), 0o600); err != nil {
		t.Fatal(err)
	}
	link := mkSession(t, st, "20260926-151000-dddddd")
	if err := os.Symlink(secret, filepath.Join(link.Dir, homeKeyFile)); err != nil {
		t.Fatal(err)
	}
	dir := mkSession(t, st, "20260926-152000-dddddd")
	if err := os.Mkdir(filepath.Join(dir.Dir, homeKeyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Session{"FIFO": fifo, "symlink": link, "ディレクトリ": dir} {
		if got, err := st.HomeKey(s); err == nil || got != "" {
			t.Errorf("%s の記録: HomeKey = %q, %v (want error。symlink の先を読んではいけない)", name, got, err)
		}
	}
}

// Create は、repo のキーを、実 path (symlink を解決したもの) から作り、セッションのディレクトリの直下 (檻に bind されない場所) に、0600 で記録する。
// 同じ repo は (別の名前 = symlink で指しても) 同じキー、別の repo は別のキー。
func TestCreateRecordsHomeKey(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	other := newSrcRepo(t)
	st, _, _ := testStore(t)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(src.Dir, link); err != nil {
		t.Fatal(err)
	}

	a1, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := st.Create(ctxT(t), CreateOptions{Repo: link}) // symlink 越し: 同じ repo
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Create(ctxT(t), CreateOptions{Repo: other.Dir})
	if err != nil {
		t.Fatal(err)
	}
	key := func(s *Session) string {
		t.Helper()
		k, err := st.HomeKey(s)
		if err != nil || k == "" {
			t.Fatalf("HomeKey(%s) = %q, %v", s.ID, k, err)
		}
		return k
	}
	if key(a1) != RepoKey(src.Dir) { // newSrcRepo の Dir は、symlink を解決した path
		t.Errorf("キー = %q, want RepoKey(%s) = %q", key(a1), src.Dir, RepoKey(src.Dir))
	}
	if key(a1) != key(a2) {
		t.Errorf("同じ repo (symlink 越し) のキーが違う: %q・%q", key(a1), key(a2))
	}
	if key(a1) == key(b) {
		t.Errorf("別の repo のキーが同じ: %q", key(b))
	}
	path := filepath.Join(a1.Dir, homeKeyFile)
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("記録の権限 = %v, %v, want 0600", fi, err)
	}
	for _, bound := range []string{a1.Clone, a1.Run, a1.Export} { // 檻に見せる場所の中には、置かない
		if exists(filepath.Join(bound, homeKeyFile)) {
			t.Errorf("記録が、檻に bind される %s の中にある", bound)
		}
	}

	// repo が移動・改名されても、記録は変わらない (--session は、記録を使う)。
	moved := src.Dir + "-moved"
	if err := os.Rename(src.Dir, moved); err != nil {
		t.Fatal(err)
	}
	if key(a1) != RepoKey(src.Dir) {
		t.Error("repo を移動したら、記録が変わった")
	}
}

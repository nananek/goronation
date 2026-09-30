//go:build unix

package credfile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/core/credential"
)

// tok は、値の目印 (印字できる ASCII 1 語)。error・出力のどこにも、現れてはならない。
const tok = "github_pat_11AAAAAAA0SECRETSECRETSECRET_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// noLeak は、err の文言 (%v・%+v・%#v) に、値 (全体・目印) が含まれていないことを確かめる。
func noLeak(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, out := range []string{err.Error(), fmt.Sprintf("%v|%+v|%#v", err, err, err)} {
		for _, part := range []string{tok, "SECRETSECRET", "github_pat_", "abcdefghij"} {
			if strings.Contains(out, part) {
				t.Errorf("%s: error に値 (%q) が出た: %s", what, part, out)
				return
			}
		}
	}
}

// newStore は、空の状態ディレクトリの Store。
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	state := t.TempDir()
	st, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	return st, state
}

// put は、資格情報のディレクトリ (0700) と、ファイル (mode) を、直接作る (Save を通さない)。
func put(t *testing.T, st *Store, name, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(st.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := st.Path(name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSaveAndToken(t *testing.T) {
	st, state := newStore(t)
	if st.Dir() != filepath.Join(state, "credentials") || st.Path("github") != filepath.Join(state, "credentials", "github") {
		t.Errorf("Dir・Path = %q・%q", st.Dir(), st.Path("github"))
	}
	ctx := context.Background()

	// 無い: ディレクトリも無い。
	if _, err := st.Token(ctx, "github"); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("無い (ディレクトリも無い): %v, want ErrNotFound", err)
	}
	if _, err := os.Lstat(st.Dir()); err == nil {
		t.Fatal("Token が、ディレクトリを作った")
	}

	if err := st.Save("github", credential.New(tok)); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{st.Dir(): 0o700, st.Path("github"): 0o600} {
		if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != want || fi.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s の権限 = %v, %v, want %04o", path, fi, err, want)
		}
	}
	if b, err := os.ReadFile(st.Path("github")); err != nil || string(b) != tok+"\n" {
		t.Errorf("ファイルの中身が違う (%v)", err)
	}
	s, err := st.Token(ctx, "github")
	if err != nil || s.Reveal() != tok {
		t.Fatalf("Token = %v, %v", s, err)
	}
	if got := fmt.Sprintf("%v %+v %#v", s, s, s); strings.Contains(got, "SECRET") {
		t.Errorf("Token の返す Secret に、値が出た: %s", got)
	}
	// 無い名前は ErrNotFound (ディレクトリはある)。
	if _, err := st.Token(ctx, "other"); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("無い名前: %v", err)
	}
	// 上書き: 置き換わり、一時ファイルが残らない。
	tok2 := strings.Replace(tok, "AAAAAAA0", "BBBBBBB1", 1)
	if err := st.Save("github", credential.New(tok2)); err != nil {
		t.Fatal(err)
	}
	if s, err := st.Token(ctx, "github"); err != nil || s.Reveal() != tok2 {
		t.Errorf("上書き後の Token = %v, %v", s, err)
	}
	ents, _ := os.ReadDir(st.Dir())
	if len(ents) != 1 || ents[0].Name() != "github" {
		t.Errorf("ディレクトリの中 = %v (github だけのはず。一時ファイルが残った?)", ents)
	}
	// ファイルが 0400 (書き込みなし) でも読める。
	if err := os.Chmod(st.Path("github"), 0o400); err != nil {
		t.Fatal(err)
	}
	if s, err := st.Token(ctx, "github"); err != nil || s.Reveal() != tok2 {
		t.Errorf("0400 の Token = %v, %v", s, err)
	}
	// 複数の名前は、別のファイル (表のキー)。
	if err := st.Save("vault-1", credential.New("abc123")); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.Token(ctx, "vault-1"); s.Reveal() != "abc123" {
		t.Error("別の名前の値が違う")
	}
}

// umask に依らず、0600 (と、ディレクトリは 0700 か、それより厳しい)。
func TestSaveModeIgnoresUmask(t *testing.T) {
	for _, umask := range []int{0, 0o022, 0o077, 0o277} { // 0o277 は、owner の書き込みも消す: 0600 を保証するのは、fchmod
		base := t.TempDir() // umask を変える前に作る (変えた後だと、この dir 自体が 0500 になる)
		old := syscall.Umask(umask)
		state := filepath.Join(base, "state", "goronation") // 無い親も、作る (0700)
		st, _ := New(state)
		err := st.Save("github", credential.New(tok))
		syscall.Umask(old)
		if err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(st.Path("github")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("umask %04o: ファイルの権限 = %v, %v", umask, fi, err)
		}
		for _, dir := range []string{st.Dir(), filepath.Dir(st.Dir())} { // credentials と、(無ければ作った) 状態ディレクトリ
			if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 && dir == st.Dir() {
				t.Errorf("umask %04o: %s の権限 = %v, %v, want 0700", umask, dir, fi, err)
			}
		}
		// 読み直せる (非 root でも、作ったディレクトリの中を読み書きできる: umask 0277 の 0500 のままだと、書けない)。
		if s, err := st.Token(context.Background(), "github"); err != nil || s.Reveal() != tok {
			t.Errorf("umask %04o: 保存した値を読めない: %v", umask, err)
		}
	}
}

// 権限が緩い (group・other の bit・setuid・setgid・sticky) ディレクトリ・ファイルは、読まず・書かず、ErrUnsafe。直すコマンドを示す。
func TestRefusesLooseModes(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666, 0o620, 0o602, 0o601, 0o610, 0o606, 0o600 | os.ModeSetuid, 0o600 | os.ModeSetgid, 0o600 | os.ModeSticky} {
		st, _ := newStore(t)
		path := put(t, st, "github", tok+"\n", mode)
		_, err := st.Token(ctx, "github")
		if !errors.Is(err, ErrUnsafe) {
			t.Errorf("ファイル %v: %v, want ErrUnsafe", mode, err)
			continue
		}
		noLeak(t, fmt.Sprintf("ファイル %v", mode), err)
		if !strings.Contains(err.Error(), "chmod 600 "+path) {
			t.Errorf("ファイル %v: 直すコマンドが無い: %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o755, 0o750, 0o770, 0o777, 0o701, 0o710, 0o705, 0o740, 0o700 | os.ModeSticky, 0o700 | os.ModeSetgid} {
		st, _ := newStore(t)
		put(t, st, "github", tok+"\n", 0o600)
		if err := os.Chmod(st.Dir(), mode); err != nil {
			t.Fatal(err)
		}
		_, terr := st.Token(ctx, "github")
		serr := st.Save("github", credential.New(tok))
		for what, err := range map[string]error{"Token": terr, "Save": serr} {
			if !errors.Is(err, ErrUnsafe) {
				t.Errorf("ディレクトリ %v の %s: %v, want ErrUnsafe", mode, what, err)
			}
			noLeak(t, "ディレクトリ", err)
		}
		if terr != nil && !strings.Contains(terr.Error(), "chmod 700 "+st.Dir()) {
			t.Errorf("ディレクトリ %v: 直すコマンドが無い: %v", mode, terr)
		}
		// 書かない: 元のファイルは、そのまま。
		if b, _ := os.ReadFile(st.Path("github")); string(b) != tok+"\n" {
			t.Errorf("ディレクトリ %v: 拒否した Save が、ファイルを書き換えた", mode)
		}
		os.Chmod(st.Dir(), 0o700) // t.TempDir の後始末のため
	}
}

// symlink は、辿らず、ErrUnsafe: ファイル (先が良いファイルでも)・ぶら下がった symlink・ディレクトリ。
func TestRefusesSymlinks(t *testing.T) {
	ctx := context.Background()
	st, state := newStore(t)
	outside := filepath.Join(state, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(outside, "good")
	if err := os.WriteFile(good, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(st.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"github": good, "vault-1": filepath.Join(outside, "none")} {
		if err := os.Symlink(target, st.Path(name)); err != nil {
			t.Fatal(err)
		}
		s, err := st.Token(ctx, name)
		if !errors.Is(err, ErrUnsafe) || s.Reveal() != "" {
			t.Errorf("symlink %s: %q, %v, want ErrUnsafe・値なし", name, s.Reveal(), err)
		}
		noLeak(t, "symlink "+name, err)
	}
	// Save は、symlink を、辿らずに置き換える (先のファイルは、変わらない)。
	other := "github_pat_" + strings.Repeat("Z", 40)
	if err := st.Save("github", credential.New(other)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(good); string(b) != tok+"\n" {
		t.Error("Save が、symlink の先のファイルを書き換えた")
	}
	if fi, err := os.Lstat(st.Path("github")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("Save 後の github = %v, %v (通常のファイルのはず)", fi, err)
	}

	// ディレクトリ自体が symlink (先は 0700 の良いディレクトリ)。
	st2, state2 := newStore(t)
	real := filepath.Join(state2, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "github"), []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, st2.Dir()); err != nil {
		t.Fatal(err)
	}
	_, terr := st2.Token(ctx, "github")
	serr := st2.Save("github", credential.New(other))
	if !errors.Is(terr, ErrUnsafe) || !errors.Is(serr, ErrUnsafe) {
		t.Errorf("symlink のディレクトリ: Token=%v Save=%v, want ErrUnsafe", terr, serr)
	}
	if b, _ := os.ReadFile(filepath.Join(real, "github")); string(b) != tok+"\n" {
		t.Error("Save が、symlink のディレクトリの先に書いた")
	}
}

// 通常のファイルでないもの (FIFO・ディレクトリ・ソケット) は、開かず (FIFO で止まらず)、ErrUnsafe。ハードリンクも。
func TestRefusesNonRegularAndHardlinks(t *testing.T) {
	ctx := context.Background()
	within := func(what string, f func() error) error {
		t.Helper()
		ch := make(chan error, 1)
		go func() { ch <- f() }()
		select {
		case err := <-ch:
			return err
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: 5 秒たっても戻らない (FIFO を開いて止まった?)", what)
			return nil
		}
	}

	st, _ := newStore(t)
	if err := os.MkdirAll(st.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(st.Path("github"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := within("FIFO", func() error { _, err := st.Token(ctx, "github"); return err }); !errors.Is(err, ErrUnsafe) {
		t.Errorf("FIFO: %v, want ErrUnsafe", err)
	}

	st, _ = newStore(t)
	if err := os.MkdirAll(filepath.Join(st.Dir(), "github", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Token(ctx, "github"); !errors.Is(err, ErrUnsafe) {
		t.Errorf("ディレクトリ: %v, want ErrUnsafe", err)
	}
	if err := st.Save("github", credential.New(tok)); err == nil { // ディレクトリを、ファイルで置き換えない (rename が失敗する)
		t.Error("ディレクトリの上に Save できた")
	} else {
		noLeak(t, "ディレクトリへの Save", err)
	}
	if ents, _ := os.ReadDir(st.Dir()); len(ents) != 1 {
		t.Errorf("失敗した Save が、一時ファイルを残した: %v", ents)
	}

	st, _ = newStore(t)
	if err := os.MkdirAll(st.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// ソケットの path は 108 バイト未満に収める (t.TempDir は長い): 短い dir に作り、その dir を、資格情報のディレクトリの名前にする。
	l, err := net.Listen("unix", st.Path("github"))
	if err != nil {
		t.Skipf("unix ソケットを作れない: %v", err)
	}
	defer l.Close()
	if _, err := st.Token(ctx, "github"); !errors.Is(err, ErrUnsafe) {
		t.Errorf("ソケット: %v, want ErrUnsafe", err)
	}

	st, _ = newStore(t)
	path := put(t, st, "github", tok+"\n", 0o600)
	link := filepath.Join(filepath.Dir(filepath.Dir(path)), "hardlink")
	if err := os.Link(path, link); err != nil {
		t.Skipf("ハードリンクを作れない: %v", err)
	}
	_, herr := st.Token(ctx, "github")
	if !errors.Is(herr, ErrUnsafe) || !strings.Contains(herr.Error(), "ハードリンク") {
		t.Errorf("ハードリンク: %v, want ErrUnsafe (ハードリンク)", herr)
	}
	noLeak(t, "ハードリンク", herr)
}

// 所有者が、実行した利用者でなければ、ErrUnsafe (root で動くときだけ確かめられる)。
func TestRefusesOtherOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown には root が要る")
	}
	ctx := context.Background()
	st, _ := newStore(t)
	path := put(t, st, "github", tok+"\n", 0o600)
	if err := os.Chown(path, 65534, -1); err != nil {
		t.Skipf("chown できない: %v", err)
	}
	_, err := st.Token(ctx, "github")
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "所有者") {
		t.Errorf("他の利用者のファイル: %v, want ErrUnsafe (所有者)", err)
	}
	noLeak(t, "所有者", err)

	st, _ = newStore(t)
	put(t, st, "github", tok+"\n", 0o600)
	if err := os.Chown(st.Dir(), 65534, -1); err != nil {
		t.Skipf("chown できない: %v", err)
	}
	if _, err := st.Token(ctx, "github"); !errors.Is(err, ErrUnsafe) {
		t.Errorf("他の利用者のディレクトリ: %v, want ErrUnsafe", err)
	}
	if err := st.Save("github", credential.New(tok)); !errors.Is(err, ErrUnsafe) {
		t.Errorf("他の利用者のディレクトリへの Save: %v, want ErrUnsafe", err)
	}
	os.Chown(st.Dir(), 0, -1)
}

// 中身が値の形でなければ ErrInvalid。値 (目印) は、error に出ない。
func TestRefusesInvalidContent(t *testing.T) {
	ctx := context.Background()
	ok := []string{tok, tok + "\n", "a", "x\n"}
	for _, content := range ok {
		st, _ := newStore(t)
		put(t, st, "github", content, 0o600)
		if s, err := st.Token(ctx, "github"); err != nil || s.Reveal() != strings.TrimSuffix(content, "\n") {
			t.Errorf("%q: %v, %v", content, s, err)
		}
	}
	for name, content := range map[string]string{
		"空": "", "改行だけ": "\n", "改行が 2 つ": tok + "\n\n", "途中の改行": tok + "\n" + tok + "\n", "CRLF": tok + "\r\n",
		"空白を含む": tok + " extra\n", "先頭の空白": " " + tok + "\n", "末尾の空白": tok + " \n", "タブ": tok + "\t\n", "NUL": tok + "\x00\n",
		"制御文字": tok + "\x1b[31m\n", "非 ASCII": tok + "日本語\n", "DEL": tok + "\x7f\n", "1025 バイトの値": strings.Repeat("A", 1025) + "\n",
		"4097 バイト": tok + strings.Repeat("\n", 4097),
	} {
		st, _ := newStore(t)
		put(t, st, "github", content, 0o600)
		s, err := st.Token(ctx, "github")
		if !errors.Is(err, ErrInvalid) || s.Reveal() != "" {
			t.Errorf("%s: %q, %v, want ErrInvalid・値なし", name, s.Reveal(), err)
		}
		noLeak(t, name, err)
	}
	// 1024 バイトちょうどは、値として通る。
	st, _ := newStore(t)
	put(t, st, "github", strings.Repeat("A", 1024)+"\n", 0o600)
	if s, err := st.Token(ctx, "github"); err != nil || len(s.Reveal()) != 1024 {
		t.Errorf("1024 バイト: %v", err)
	}
}

// Save は、値の形でないもの (空・空白・改行・NUL・非 ASCII・長すぎる) を書かず、何も作らない。error に値を出さない。
func TestSaveRefusesInvalidValue(t *testing.T) {
	for name, v := range map[string]string{
		"空": "", "空白": tok + " x", "改行": tok + "\n", "NUL": tok + "\x00", "非 ASCII": tok + "あ", "長すぎる": strings.Repeat("A", 1025), "制御文字": tok + "\x01",
	} {
		st, _ := newStore(t)
		err := st.Save("github", credential.New(v))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
		noLeak(t, name, err)
		if _, err := os.Lstat(st.Dir()); err == nil {
			t.Errorf("%s: 断ったのに、ディレクトリを作った", name)
		}
	}
}

// 名前が形でなければ ErrInvalidName で、path に触れない (traversal・区切り・大文字・空)。
func TestInvalidNamesTouchNothing(t *testing.T) {
	st, state := newStore(t)
	for _, name := range []string{"", "..", "../x", "a/b", "GitHub", "a b", ".hidden", "a\x00b", strings.Repeat("a", 33), "github/../x", "/etc/passwd"} {
		if _, err := st.Token(context.Background(), name); !errors.Is(err, credential.ErrInvalidName) {
			t.Errorf("Token(%q) = %v, want ErrInvalidName", name, err)
		}
		if err := st.Save(name, credential.New(tok)); !errors.Is(err, credential.ErrInvalidName) {
			t.Errorf("Save(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	if ents, _ := os.ReadDir(state); len(ents) != 0 {
		t.Errorf("形の違う名前が、状態ディレクトリに何かを作った: %v", ents)
	}
}

func TestNewRejectsBadStateDir(t *testing.T) {
	for _, dir := range []string{"", "relative/dir", "/a/../b", "/a//b", "/a/b/", "/a\nb", "/a\x00b", "/a\x1bb"} {
		if _, err := New(dir); err == nil {
			t.Errorf("New(%q) が通った", dir)
		}
	}
	if _, err := New("/tmp/state"); err != nil {
		t.Error(err)
	}
}

func TestTokenHonorsContext(t *testing.T) {
	st, _ := newStore(t)
	st.Save("github", credential.New(tok))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.Token(ctx, "github"); !errors.Is(err, context.Canceled) {
		t.Errorf("取り消した ctx: %v, want context.Canceled", err)
	}
}

// 書いている間 (rename の直前) に、ディレクトリが symlink に差し替えられたら、別の場所へ置かず、ErrUnsafe。
func TestSaveDetectsDirectorySwap(t *testing.T) {
	st, state := newStore(t)
	if err := st.Save("github", credential.New(tok)); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(state, "attacker")
	if err := os.Mkdir(attacker, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(state, "moved")
	beforeRename = func() {
		os.Rename(st.Dir(), moved)
		os.Symlink(attacker, st.Dir())
	}
	defer func() { beforeRename = nil }()
	err := st.Save("github", credential.New(strings.Replace(tok, "AAAAAAA0", "CCCCCCC2", 1)))
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("差し替え: %v, want ErrUnsafe", err)
	}
	noLeak(t, "差し替え", err)
	if ents, _ := os.ReadDir(attacker); len(ents) != 0 {
		t.Errorf("差し替えた先に、ファイルが置かれた: %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(moved, "github")); string(b) != tok+"\n" {
		t.Error("元のファイルが、書き換わった")
	}
	if ents, _ := os.ReadDir(moved); len(ents) != 1 {
		t.Errorf("一時ファイルが残った: %v", ents)
	}
}

// 書き込みは原子的: 読み手は、古い値か新しい値のどちらかを読み、途中の値・エラーを見ない。
func TestSaveIsAtomicForReaders(t *testing.T) {
	st, _ := newStore(t)
	a := "github_pat_" + strings.Repeat("A", 60)
	b := "github_pat_" + strings.Repeat("B", 80)
	if err := st.Save("github", credential.New(a)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan string, 16)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s, err := st.Token(context.Background(), "github")
				if err != nil || (s.Reveal() != a && s.Reveal() != b) {
					select {
					case errs <- fmt.Sprintf("読み手: %v (値の長さ %d)", err, len(s.Reveal())):
					default:
					}
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		v := a
		if i%2 == 0 {
			v = b
		}
		if err := st.Save("github", credential.New(v)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// 読む間 (開いた後・検査の前) に、書き手が置き換えたら、読み直して、新しい値を返す。置き換わり続けるなら、ErrUnsafe (無限に待たない)。
func TestTokenRetriesWhenReplacedWhileReading(t *testing.T) {
	st, _ := newStore(t)
	a := "github_pat_" + strings.Repeat("A", 60)
	b := "github_pat_" + strings.Repeat("B", 60)
	if err := st.Save("github", credential.New(a)); err != nil {
		t.Fatal(err)
	}
	defer func() { afterOpen = nil }()

	fired := 0
	afterOpen = func() { // 開いた fd の名前を、別のファイルで置き換える (開いた方は、Nlink 0 になる)。1 回だけ
		if fired++; fired == 1 {
			if err := st.Save("github", credential.New(b)); err != nil {
				t.Error(err)
			}
		}
	}
	s, err := st.Token(context.Background(), "github")
	if err != nil || s.Reveal() != b || fired < 2 {
		t.Errorf("置き換わりの後の Token = %v, %v (afterOpen %d 回。読み直して、新しい値 b のはず)", s, err, fired)
	}

	fired = 0
	n := 0
	afterOpen = func() { // 毎回置き換わる
		fired++
		n++
		v := a
		if n%2 == 0 {
			v = b
		}
		if err := st.Save("github", credential.New(v)); err != nil {
			t.Error(err)
		}
	}
	_, err = st.Token(context.Background(), "github")
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "書き換わり続けている") || fired != maxAttempts {
		t.Errorf("置き換わり続けるとき: %v (afterOpen %d 回。ErrUnsafe・%d 回のはず)", err, fired, maxAttempts)
	}
	noLeak(t, "置き換わり続ける", err)
}

func TestNewSizedRaisesValueLimitOnly(t *testing.T) {
	dir := t.TempDir()
	big, err := NewSized(dir, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Repeat("a", 5000)
	if err := big.Save("wide", credential.New(value)); err != nil {
		t.Fatalf("大きい上限の Store が、5000 バイトを保存できない: %v", err)
	}
	if s, err := big.Token(context.Background(), "wide"); err != nil || s.Reveal() != value {
		t.Fatalf("Token: %v", err)
	}
	// 既定の Store は、1024 バイトを超える値を、保存も読みもしない。
	def, _ := New(dir)
	if err := def.Save("wide2", credential.New(value)); err == nil {
		t.Fatal("既定の Store が、5000 バイトを保存した")
	}
	if _, err := def.Token(context.Background(), "wide"); err == nil {
		t.Fatal("既定の Store が、大きい値を読んだ")
	}
	// 値の形の検査 (印字できる ASCII 1 語) は、上限に関わらず効く。
	if err := big.Save("sp", credential.New("a b")); err == nil {
		t.Fatal("空白を含む値を保存できた")
	}
	for _, n := range []int{0, -1, 64<<10 + 1} {
		if _, err := NewSized(dir, n); err == nil {
			t.Fatalf("NewSized(%d) が成功した", n)
		}
	}
}

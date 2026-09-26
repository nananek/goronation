package session

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nananek/goronation/sandbox/bwrap"
)

const requireEnv = "GORO_REQUIRE_BWRAP"

// bwrapProbe は、bwrap で、檻の中の /usr/bin/true を実行できるかの確認結果 (プロセスの中で 1 回だけ確かめる)。
var bwrapProbe struct {
	once   sync.Once
	reason string // 空なら使える
}

// needBwrap は、bwrap を使えなければ、テストを skip する。GORO_REQUIRE_BWRAP=1 なら、skip でなく fail する。
func needBwrap(t *testing.T) {
	t.Helper()
	bwrapProbe.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c, err := bwrap.Start(ctx, bwrap.Spec{
			Host:     bwrap.Host{Home: "/nonexistent-home"},
			Symlinks: []bwrap.Symlink{{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"}},
			Binds:    []bwrap.Bind{{Src: "/usr", Dst: "/usr"}},
			Cmd:      []string{"/usr/bin/true"},
		})
		if err == nil {
			err = c.Wait()
		}
		if err != nil {
			bwrapProbe.reason = err.Error()
		}
	})
	if bwrapProbe.reason == "" {
		return
	}
	if os.Getenv(requireEnv) == "1" {
		t.Fatalf("%s=1 だが bwrap を使えない: %s", requireEnv, bwrapProbe.reason)
	}
	t.Skipf("bwrap を使えないため skip する (%s=1 で必須になる): %s", requireEnv, bwrapProbe.reason)
}

// needGit は、ホストの git (テストが、元のリポジトリを作る・利用者の git fetch を模す・罠の対照実験に使う) が無ければ skip する。
func needGit(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(gitBin); err != nil {
		t.Skipf("%s が無い: %v", gitBin, err)
	}
}

// hostEnv は、ホストで git を実行するときの環境 (設定を固定する: システムと利用者の設定を読まない)。
func hostEnv(home string) []string {
	return []string{
		"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
}

// hostGit は、ホストで、dir を作業ディレクトリにして git args を実行し、出力を返す (テストの準備と、対照実験だけに使う。
// 本体は、ホストで git を実行しない)。失敗したら、テストを止める。
func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := hostGitErr(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func hostGitErr(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(gitBin, append([]string{"-c", "user.name=t", "-c", "user.email=t@e.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = hostEnv(t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// testStore は、偽の HOME の下に状態ディレクトリを持つ Store と、その偽の HOME、状態ディレクトリを返す。
// 状態ディレクトリの名前は、空白と引用符を含む (取り込みのコマンドの、引用の確認のため)。
func testStore(t *testing.T) (st *Store, home, state string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(dir, "home")
	state = filepath.Join(home, ".local", "state", "go ro'x")
	st, err = NewStore(state, bwrap.Host{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return st, home, state
}

// srcRepo は、元のリポジトリと、罠が実行されたときの目印を置くディレクトリ。
type srcRepo struct {
	Dir     string // 元のリポジトリ (作業ツリー)
	Markers string // 罠が実行されると、ここに ran-<名前> のファイルができる。檻には見えない場所
}

// trapNames は、仕込む罠の名前。
var trapNames = []string{"fsmonitor", "alias", "textconv", "clean", "hook", "packhook", "hooksdir"}

// writeTraps は、罠のスクリプトを markers に作る。実行されると markers/ran-<name> を作る。
func writeTraps(t *testing.T, markers string) {
	t.Helper()
	for _, n := range trapNames {
		body := "#!/bin/sh\n: > '" + markers + "/ran-" + n + "'\ncat \"${1:-/dev/null}\" 2>/dev/null\nexit 0\n"
		if err := os.WriteFile(filepath.Join(markers, "trap-"+n+".sh"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// trapConfig は、.git/config に足す、罠の設定 (実行すると、ホストに目印を作るもの)。
func trapConfig(markers string) string {
	tr := func(n string) string { return markers + "/trap-" + n + ".sh" }
	return "[core]\n\tfsmonitor = " + tr("fsmonitor") + "\n\thooksPath = " + markers + "/hooks\n" +
		"[alias]\n\tevil = !" + tr("alias") + "\n" +
		"[diff \"evil\"]\n\ttextconv = " + tr("textconv") + "\n" +
		"[filter \"evil\"]\n\tclean = " + tr("clean") + "\n\tsmudge = " + tr("clean") + "\n" +
		"[uploadpack]\n\tpackObjectsHook = " + tr("packhook") + "\n"
}

// hookNames は、罠にする hooks。
var hookNames = []string{"pre-commit", "post-checkout", "post-merge", "reference-transaction", "post-commit"}

// writeHookTraps は、hooks のディレクトリに、実行されると目印を作る hook を作る。
func writeHookTraps(t *testing.T, hooksDir, markers string) {
	t.Helper()
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range hookNames {
		body := "#!/bin/sh\n: > '" + markers + "/ran-hooksdir'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(hooksDir, h), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// newSrcRepo は、罠を仕込んだ元のリポジトリを作る (ホストの git で。目印のディレクトリは、檻に見えない場所)。
// コミット済みの内容 (README・symlink・.gitmodules・.gitattributes・大きなファイル・サブディレクトリ) と、コミットしていない変更
// (README の編集・未追跡の secret.env・ステージしただけの staged.txt) を持つ。
func newSrcRepo(t *testing.T) srcRepo {
	t.Helper()
	needGit(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := srcRepo{Dir: filepath.Join(base, "src"), Markers: filepath.Join(base, "markers")}
	for _, d := range []string{r.Dir, r.Markers} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(r.Dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hostGit(t, r.Dir, "init", "-q", "-b", "main", ".")
	write("README.md", "# src\n")
	write("notes.txt", "notes\n")
	write("subdir/file.txt", "in subdir\n")
	write(".gitmodules", "[submodule \"sub\"]\n\tpath = sub\n\turl = https://example.invalid/sub.git\n")
	write(".gitattributes", "*.txt diff=evil filter=evil\n")
	big := make([]byte, 8<<20)
	for i := range big {
		big[i] = byte(i*131 + i>>8)
	}
	if err := os.WriteFile(filepath.Join(r.Dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(r.Dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(r.Dir, "abs-link")); err != nil {
		t.Fatal(err)
	}
	hostGit(t, r.Dir, "add", "-A")
	hostGit(t, r.Dir, "commit", "-q", "-m", "init")
	write("notes.txt", "notes\nsecond\n")
	hostGit(t, r.Dir, "commit", "-q", "-a", "-m", "second")
	// ステージしただけ (コミットしていない) のファイル。罠の設定を入れる前に、git add しておく (後だと、罠が発火する)。
	write("staged.txt", "STAGED-NOT-COMMITTED\n")
	hostGit(t, r.Dir, "add", "staged.txt")

	// 罠 (.git/config・hooks)。git を実行せずに、ファイルとして書く (罠が、準備の git で発火しないように)。
	writeTraps(t, r.Markers)
	writeHookTraps(t, filepath.Join(r.Markers, "hooks"), r.Markers)
	writeHookTraps(t, filepath.Join(r.Dir, ".git", "hooks"), r.Markers)
	cfg, err := os.OpenFile(filepath.Join(r.Dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.WriteString(trapConfig(r.Markers)); err != nil {
		t.Fatal(err)
	}
	cfg.Close()

	// コミットしていない変更 (作業ツリーの編集と、未追跡のファイル)。
	write("README.md", "# src\nUNCOMMITTED-EDIT\n")
	write("secret.env", "SECRET-UNTRACKED\n")
	return r
}

// markersHit は、markers に、ran-<name> が (罠が実行された目印として) できているものの名前。
func markersHit(t *testing.T, markers string) []string {
	t.Helper()
	ents, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	var hit []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "ran-") {
			hit = append(hit, strings.TrimPrefix(e.Name(), "ran-"))
		}
	}
	sort.Strings(hit)
	return hit
}

// treeHash は、dir の下のすべてのファイル (種類・モード・中身・symlink の先) のハッシュ。ホストの元のリポジトリが、
// 変わらなかったことの確認に使う。
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%o\x00", rel, info.Mode())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			fmt.Fprintf(h, "->%s\x00", target)
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// cageSh は、セッションの clone を rw で (ホストと同じ path で) bind した檻の中で、clone を cwd にして、sh -c script を実行する (檻の中の AI が、clone に
// コミットする・.git/config や hooks を書く、を模す)。失敗したら、テストを止める。
func cageSh(t *testing.T, st *Store, sess *Session, script string) {
	t.Helper()
	spec := st.gitSpec([]bwrap.Bind{st.bind(sess.Clone, true)})
	spec.Cmd = []string{"/usr/bin/sh", "-c", script}
	spec.Chdir = sess.Clone
	var out boundedBuffer
	spec.Stdout, spec.Stderr = &out, &out
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := bwrap.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatalf("檻の中の sh が失敗した: %v\n%s", err, out.text())
	}
}

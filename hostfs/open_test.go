package hostfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRoot は、files (root からの相対 path → 中身) を作った一時ディレクトリを、os.Root として開く。dir は、その絶対 path。
func newRoot(t *testing.T, files map[string]string) (*os.Root, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

// within は、fn が d 以内に終わることを確かめる (止まる実装が、テスト全体を道連れにしないため)。
func within(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s 以内に終わらない (止まっている)", d)
	}
}

func TestReadRegularFile(t *testing.T) {
	root, _ := newRoot(t, map[string]string{"a.bundle": "hello", "d/e/f.txt": "nested", "empty": ""})
	for name, want := range map[string]string{"a.bundle": "hello", "d/e/f.txt": "nested", "empty": ""} {
		got, err := ReadFile(root, name, 100)
		if err != nil || string(got) != want {
			t.Errorf("ReadFile(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
	f, err := Open(root, "a.bundle", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if b, _ := io.ReadAll(f); string(b) != "hello" {
		t.Errorf("Open したファイルの中身 = %q, want hello (offset 0 から読める)", b)
	}
}

func TestSizeLimit(t *testing.T) {
	root, _ := newRoot(t, map[string]string{"five": "12345", "zero": ""})
	for _, tc := range []struct {
		name string
		max  int64
		ok   bool
	}{{"five", 5, true}, {"five", 4, false}, {"five", 0, false}, {"zero", 0, true}, {"five", 100, true}} {
		_, err := ReadFile(root, tc.name, tc.max)
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrTooLarge)) {
			t.Errorf("ReadFile(%q, max %d) = %v, want ok=%v (ErrTooLarge)", tc.name, tc.max, err, tc.ok)
		}
		f, err := Open(root, tc.name, tc.max)
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrTooLarge)) {
			t.Errorf("Open(%q, max %d) = %v, want ok=%v (ErrTooLarge)", tc.name, tc.max, err, tc.ok)
		}
		if f != nil {
			f.Close()
		}
	}
	if _, err := Open(root, "five", -1); err == nil || !strings.Contains(err.Error(), "負") {
		t.Errorf("負の上限: %v, want 「負」の error", err)
	}
	if _, err := ReadFile(root, "zero", -1); err == nil || !strings.Contains(err.Error(), "負") {
		t.Errorf("ReadFile の負の上限: %v, want 「負」の error", err)
	}
}

func TestRejectsNamesOutsideRoot(t *testing.T) {
	root, dir := newRoot(t, map[string]string{"in.txt": "in"})
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "..", "../outside.txt", "a/../../outside.txt", "/etc/passwd", outside, "/"} {
		if _, err := Open(root, name, 100); err == nil || !strings.Contains(err.Error(), "相対 path") {
			t.Errorf("Open(%q) = %v, want 「相対 path ではない」の error", name, err)
		}
	}
	if _, err := Open(root, ".", 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Open(.) = %v, want ErrNotRegular (root 自体はディレクトリ)", err)
	}
}

func TestMissingFile(t *testing.T) {
	root, _ := newRoot(t, nil)
	if _, err := Open(root, "none", 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("存在しない名前: %v, want fs.ErrNotExist", err)
	}
	if _, err := Open(root, "no/such/dir/file", 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("存在しない途中のディレクトリ: %v, want fs.ErrNotExist", err)
	}
}

func TestRejectsSymlinks(t *testing.T) {
	root, dir := newRoot(t, map[string]string{"real.txt": "real", "sub/real2.txt": "real2"})
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := func(name, target string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("symlink を作れない: %v", err)
		}
	}
	link("to-file", "real.txt")              // root の中の通常ファイルへ
	link("to-outside", outside)              // root の外へ (絶対)
	link("to-outside-rel", "../outside.txt") // root の外へ (相対)
	link("dangling", "nowhere")              // 存在しない先へ
	link("to-dir", "sub")                    // ディレクトリへ
	link("loop", "loop")                     // 自分自身へ
	link("dirlink", "sub")                   // 途中に使う
	for _, name := range []string{"to-file", "to-outside", "to-outside-rel", "dangling", "to-dir", "loop"} {
		f, err := Open(root, name, 100)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("Open(%q) = %v, want ErrNotRegular (symlink は辿らない)", name, err)
		}
	}
	// 途中のディレクトリが symlink なら、その先の通常ファイルでも拒否する。
	f, err := Open(root, "dirlink/real2.txt", 100)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, ErrNotRegular) || !strings.Contains(err.Error(), "dirlink") {
		t.Errorf("途中が symlink: %v, want ErrNotRegular (dirlink)", err)
	}
	// 2 階層目が symlink でも、拒否する (途中の全部を確かめる)。
	if err := os.Symlink("../sub", filepath.Join(dir, "sub", "inner")); err == nil {
		f, err = Open(root, "sub/inner/real2.txt", 100)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrNotRegular) || !strings.Contains(err.Error(), "sub/inner") {
			t.Errorf("2 階層目が symlink: %v, want ErrNotRegular (sub/inner)", err)
		}
	}
	// 途中が本物のディレクトリなら通る。
	if b, err := ReadFile(root, "sub/real2.txt", 100); err != nil || string(b) != "real2" {
		t.Errorf("ReadFile(sub/real2.txt) = %q, %v", b, err)
	}
}

func TestRejectsDirectory(t *testing.T) {
	root, _ := newRoot(t, map[string]string{"d/x": "x"})
	if _, err := Open(root, "d", 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Open(ディレクトリ) = %v, want ErrNotRegular", err)
	}
}

// TestSwapDuringOpen は、確認と open の間・open と確認の間に、名前が差し替えられても、拒否する (開いたものが、名前そのもの)。
func TestSwapDuringOpen(t *testing.T) {
	type swap struct {
		name  string
		stage string
		do    func(t *testing.T, dir string)
		want  error
	}
	replace := func(t *testing.T, dir string, mk func(p string)) {
		t.Helper()
		p := filepath.Join(dir, "f")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		mk(p)
	}
	for _, tc := range []swap{
		{"open の後に、別の通常ファイルへ", stageAfterOpen, func(t *testing.T, dir string) {
			replace(t, dir, func(p string) { os.WriteFile(p, []byte("other"), 0o644) })
		}, ErrChanged},
		{"open の後に、symlink へ", stageAfterOpen, func(t *testing.T, dir string) {
			replace(t, dir, func(p string) { os.Symlink(filepath.Join(dir, "elsewhere"), p) })
		}, ErrChanged},
		{"open の後に、ディレクトリへ", stageAfterOpen, func(t *testing.T, dir string) {
			replace(t, dir, func(p string) { os.Mkdir(p, 0o755) })
		}, ErrChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := newRoot(t, map[string]string{"f": "orig", "elsewhere": "elsewhere"})
			var err error
			within(t, 5*time.Second, func() {
				var f *os.File
				f, err = open(root, "f", 100, func(stage string) {
					if stage == tc.stage {
						tc.do(t, dir)
					}
				})
				if f != nil {
					f.Close()
				}
			})
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("open の前に symlink へ (確認の後)。symlink は辿らずに拒否", func(t *testing.T) {
		root, dir := newRoot(t, map[string]string{"f": "orig", "elsewhere": "elsewhere"})
		var f *os.File
		var err error
		within(t, 5*time.Second, func() {
			f, err = open(root, "f", 100, func(stage string) {
				if stage == stageBeforeOpen {
					os.Remove(filepath.Join(dir, "f"))
					os.Symlink("elsewhere", filepath.Join(dir, "f"))
				}
			})
		})
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrChanged) && !errors.Is(err, ErrNotRegular) {
			t.Errorf("error = %v, want ErrChanged か ErrNotRegular (symlink を辿った先を、通してはいけない)", err)
		}
		if err == nil {
			t.Error("symlink に差し替えられたのに、開けた")
		}
	})
}

// TestFileGrowsWhileReading は、確認の後、読む前に、ファイルが伸びても、上限を超えて読まないことを確認する。
func TestFileGrowsWhileReading(t *testing.T) {
	root, dir := newRoot(t, map[string]string{"f": "12345"})
	var err error
	var data []byte
	within(t, 5*time.Second, func() {
		data, err = readFile(root, "f", 5, func(stage string) {
			if stage == stageBeforeRead {
				fh, e := os.OpenFile(filepath.Join(dir, "f"), os.O_WRONLY|os.O_APPEND, 0)
				if e != nil {
					t.Error(e)
					return
				}
				defer fh.Close()
				fh.WriteString(strings.Repeat("x", 1000))
			}
		})
	})
	if !errors.Is(err, ErrTooLarge) || data != nil {
		t.Errorf("ReadFile = %q, %v, want ErrTooLarge (読む間に伸びた)", data, err)
	}
}

// TestKindOf は、ファイルの種類の分類 (エラー文に出る名前) を確認する。デバイスは、権限が無いと作れないので、ここで補う。
func TestKindOf(t *testing.T) {
	for mode, want := range map[fs.FileMode]string{
		0o644: "通常のファイル", fs.ModeDir | 0o755: "ディレクトリ", fs.ModeSymlink: "symlink", fs.ModeNamedPipe: "FIFO",
		fs.ModeSocket: "ソケット", fs.ModeDevice: "デバイス", fs.ModeDevice | fs.ModeCharDevice: "デバイス", fs.ModeIrregular: "不明な種類",
	} {
		if got := kindOf(mode); got != want {
			t.Errorf("kindOf(%v) = %q, want %q", mode, got, want)
		}
		if mode.IsRegular() != (want == "通常のファイル") {
			t.Errorf("%v: IsRegular = %v", mode, mode.IsRegular())
		}
	}
}

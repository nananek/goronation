//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 入力の安全 (tools/archtest の M0-1 のレビューで見つかった類) のテスト。
// FIFO を開くと、writer が無いときに止まる。止まる実装が、テスト全体を道連れにしないよう、within で囲む。

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("FIFO を作れない: %v", err)
	}
}

// TestInventoryRejectsNonRegular は、名前が .go・go.mod・.md の、通常のファイルではないもの (FIFO) と、
// それを指す symlink が、開かれずに error になることを確認する。
func TestInventoryRejectsNonRegular(t *testing.T) {
	for _, name := range []string{"core/evil.go", "core/go.mod", "docs/evil.md", "core/.hidden.go"} {
		for _, viaSymlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/symlink=%v", name, viaSymlink), func(t *testing.T) {
				tr, dir := newTestTree(t, map[string]string{"core/doc.go": "package core\n"})
				if viaSymlink {
					mkfifo(t, filepath.Join(dir, "core", "fifo"))
					symlink(t, filepath.Join(dir, "core", "fifo"), filepath.Join(dir, filepath.FromSlash(name)))
				} else {
					mkfifo(t, filepath.Join(dir, filepath.FromSlash(name)))
				}
				within(t, 5*time.Second, func() {
					if inv, err := tr.inventory(); err == nil {
						t.Errorf("error を返すべき (開かずに拒否する): %+v", inv)
					}
				})
			})
		}
	}
}

// TestInventoryIgnoresIrrelevantFIFO は、読まない名前の FIFO が、開かれず (止まらず)、無視されることを確認する。
func TestInventoryIgnoresIrrelevantFIFO(t *testing.T) {
	tr, dir := newTestTree(t, map[string]string{"core/doc.go": "package core\n"})
	mkfifo(t, filepath.Join(dir, "core", "notes.txt"))
	mkfifo(t, filepath.Join(dir, "testdata", "evil.go")) // 辿らないディレクトリの中
	within(t, 5*time.Second, func() {
		inv, err := tr.inventory()
		if err != nil {
			t.Errorf("読まない名前と辿らないディレクトリは、無視するべき: %v", err)
			return
		}
		if len(inv.Go) != 1 {
			t.Errorf("Go = %v", inv.Go)
		}
	})
}

// TestInventoryRejectsSymlinks は、ツリーの中の symlink が、名前・向き先によらず、辿らずに error になることを
// 確認する。向き先が FIFO なら開かず、/proc を指すなら (プロセスごとに別のものに解決される) 読まない。
func TestInventoryRejectsSymlinks(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"通常のファイルを指す .go": func(t *testing.T, dir string) {
			symlink(t, "doc.go", filepath.Join(dir, "core", "link.go"))
		},
		"読まない名前 (.txt)": func(t *testing.T, dir string) {
			symlink(t, "doc.go", filepath.Join(dir, "core", "link.txt"))
		},
		"ディレクトリを指す": func(t *testing.T, dir string) {
			symlink(t, "sub", filepath.Join(dir, "core", "alias"))
		},
		"壊れた": func(t *testing.T, dir string) {
			symlink(t, "nowhere", filepath.Join(dir, "core", "dangling.go"))
		},
		"root の外を指す": func(t *testing.T, dir string) {
			symlink(t, "/etc/passwd", filepath.Join(dir, "core", "passwd.go"))
		},
		"/proc/self/cwd を通る (プロセスごとに別のものに解決される)": func(t *testing.T, dir string) {
			symlink(t, "/proc/self/cwd/x.go", filepath.Join(dir, "core", "proc.go"))
		},
		"/dev を指す": func(t *testing.T, dir string) {
			symlink(t, "/dev/zero", filepath.Join(dir, "core", "zero.go"))
		},
		"FIFO を指す (開かない)": func(t *testing.T, dir string) {
			mkfifo(t, filepath.Join(dir, "fifo"))
			symlink(t, "../fifo", filepath.Join(dir, "core", "fifo.go"))
		},
		"docs/reference": func(t *testing.T, dir string) {
			symlink(t, "../core", filepath.Join(dir, "docs", "reference"))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			tr, dir := newTestTree(t, map[string]string{"core/doc.go": "package core\n", "core/sub/x.go": "package sub\n", "docs/adr/README.md": "x\n"})
			setup(t, dir)
			within(t, 5*time.Second, func() {
				if inv, err := tr.inventory(); err == nil {
					t.Errorf("error を返すべき: %+v", inv)
				}
			})
		})
	}

	t.Run("辿らないディレクトリの名前の symlink は、無視する", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"core/doc.go": "package core\n"})
		symlink(t, "../core", filepath.Join(dir, "testdata"))
		symlink(t, "../core", filepath.Join(dir, "vendor"))
		symlink(t, "../core", filepath.Join(dir, ".git"))
		symlink(t, "../core", filepath.Join(dir, "_x"))
		if _, err := tr.inventory(); err != nil {
			t.Errorf("辿らない名前は無視するべき: %v", err)
		}
	})
}

// TestSymlinkFloodIsBounded は、深い tree に .go 名の symlink を多数置いても、全体の時間・項目数の予算で、
// すぐに止まる (symlink は 1 つ目で error。判定ごとに時間を払わない) ことを確認する。
// tools/archtest では、深さ 1900 に 100 本置くと、約 24 秒かかった (Check 全体の時間が非有界)。
func TestSymlinkFloodIsBounded(t *testing.T) {
	dir := t.TempDir()
	deep := dir
	for range 50 {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		symlink(t, "/proc/self/cwd/x", filepath.Join(deep, fmt.Sprintf("s%03d.go", i)))
	}
	tr := newTree(openTestRoot(t, dir), defaultLimits)
	start := time.Now()
	within(t, 5*time.Second, func() {
		if _, err := tr.inventory(); err == nil {
			t.Error("error を返すべき")
		}
	})
	if tr.entries > 2*50+600 {
		t.Errorf("entries = %d: symlink を 1 つ見つけた後も、辿り続けた", tr.entries)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("%s かかった", d)
	}
}

// TestReadFileNonRegular は、inventory を通らずに、名前が直接渡されても、readFile が FIFO で止まらず、
// symlink を辿らないことを確認する (Lstat と Open の間の差し替えへの対策)。
func TestReadFileNonRegular(t *testing.T) {
	tr, dir := newTestTree(t, map[string]string{"real.go": "package real\n", "other.go": "package other\n"})
	mkfifo(t, filepath.Join(dir, "fifo.go"))
	symlink(t, "real.go", filepath.Join(dir, "link.go"))
	symlink(t, "/etc/passwd", filepath.Join(dir, "ext.go"))

	for _, name := range []string{"fifo.go", "link.go", "ext.go", "."} {
		t.Run(name, func(t *testing.T) {
			within(t, 5*time.Second, func() {
				if b, err := tr.readFile(name); err == nil {
					t.Errorf("error を返すべき: %q", b)
				}
			})
		})
	}
}

// TestReadFileSwapped は、Lstat 後・open の前と、open の後・確認の前に、名前を別のものに差し替えても、
// 止まらず、symlink の先の別のファイルを返さないことを確認する。
func TestReadFileSwapped(t *testing.T) {
	swap := func(t *testing.T, dir, name string, replace func(p string)) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		replace(p)
	}

	t.Run("open の前に FIFO に差し替える", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"a.go": "package a\n"})
		tr.hook = func(stage, name string) {
			if stage == stageBeforeOpen {
				swap(t, dir, name, func(p string) { mkfifo(t, p) })
			}
		}
		within(t, 5*time.Second, func() {
			if b, err := tr.readFile("a.go"); err == nil {
				t.Errorf("error を返すべき: %q", b)
			}
		})
	})

	t.Run("open の前に、別のファイルへの symlink に差し替える", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"a.go": "package a\n", "secret.go": "package secret\n"})
		tr.hook = func(stage, name string) {
			if stage == stageBeforeOpen && name == "a.go" {
				swap(t, dir, name, func(p string) { symlink(t, "secret.go", p) })
			}
		}
		if b, err := tr.readFile("a.go"); err == nil {
			t.Errorf("symlink を辿って読んではいけない: %q", b)
		}
	})

	t.Run("open の後に、別のファイルへの symlink に差し替える", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"a.go": "package a\n", "secret.go": "package secret\n"})
		tr.hook = func(stage, name string) {
			if stage == stageAfterOpen && name == "a.go" {
				swap(t, dir, name, func(p string) { symlink(t, "secret.go", p) })
			}
		}
		if b, err := tr.readFile("a.go"); err == nil {
			t.Errorf("差し替えを検出すべき: %q", b)
		}
	})

	t.Run("open の後に、別の通常のファイルに差し替える", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
		tr.hook = func(stage, name string) {
			if stage == stageAfterOpen && name == "a.go" {
				swap(t, dir, name, func(p string) {
					if err := os.Rename(filepath.Join(dir, "b.go"), p); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
		if b, err := tr.readFile("a.go"); err == nil {
			t.Errorf("差し替えを検出すべき: %q", b)
		}
	})

	t.Run("差し替えが無ければ、読める (対照)", func(t *testing.T) {
		tr, _ := newTestTree(t, map[string]string{"a.go": "package a\n"})
		tr.hook = func(stage, name string) {}
		if b, err := tr.readFile("a.go"); err != nil || string(b) != "package a\n" {
			t.Errorf("readFile = %q, %v", b, err)
		}
	})
}

// TestListDirSwapped は、ディレクトリを辿る途中で、別のディレクトリへの symlink に差し替えても、
// 別のディレクトリの中身を、その名前の中身として返さないことを確認する。
func TestListDirSwapped(t *testing.T) {
	tr, dir := newTestTree(t, map[string]string{"a/x.go": "package a\n", "b/y.go": "package b\n"})
	tr.hook = func(stage, name string) {
		if stage == stageAfterOpen && name == "a" {
			if err := os.RemoveAll(filepath.Join(dir, "a")); err != nil {
				t.Fatal(err)
			}
			symlink(t, "b", filepath.Join(dir, "a"))
		}
	}
	if ents, err := tr.listDir("a"); err == nil {
		t.Errorf("差し替えを検出すべき: %+v", ents)
	}
}

// TestWriteFileNeverFollowsSymlink は、書き込みが symlink を辿らず、辿った先の別のファイルを、
// 上書き (切り詰め) しないことを確認する。os.Root は、root の中を指す symlink を辿るので、
// docs/reference/core.md を README.md への symlink にされると、素直に書くと README.md を上書きする。
func TestWriteFileNeverFollowsSymlink(t *testing.T) {
	const readme = "# 守るべき README\n"
	setup := func(t *testing.T) (*tree, string) {
		return newTestTree(t, map[string]string{"README.md": readme, "docs/reference/keep.md": "x\n"})
	}
	unchanged := func(t *testing.T, dir string) {
		t.Helper()
		if b, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil || string(b) != readme {
			t.Errorf("README.md が変わった: %q, %v", b, err)
		}
	}

	t.Run("既存の symlink", func(t *testing.T) {
		tr, dir := setup(t)
		symlink(t, "../../README.md", filepath.Join(dir, "docs", "reference", "core.md"))
		if err := tr.writeFile("docs/reference/core.md", []byte("生成物\n")); err == nil {
			t.Error("error にすべき")
		}
		unchanged(t, dir)
	})

	t.Run("壊れた symlink (辿ると、先が作られる)", func(t *testing.T) {
		tr, dir := setup(t)
		symlink(t, "../../created-through-link.md", filepath.Join(dir, "docs", "reference", "core.md"))
		if err := tr.writeFile("docs/reference/core.md", []byte("生成物\n")); err == nil {
			t.Error("error にすべき")
		}
		if _, err := os.Lstat(filepath.Join(dir, "created-through-link.md")); err == nil {
			t.Error("symlink の先に、ファイルができた")
		}
	})

	t.Run("open の前に、symlink に差し替えられる", func(t *testing.T) {
		tr, dir := setup(t)
		tr.hook = func(stage, name string) {
			if stage == stageBeforeOpen && name == "docs/reference/core.md" {
				symlink(t, "../../README.md", filepath.Join(dir, "docs", "reference", "core.md"))
			}
		}
		if err := tr.writeFile("docs/reference/core.md", []byte("生成物\n")); err == nil {
			t.Error("error にすべき")
		}
		unchanged(t, dir)
	})

	t.Run("open の後に、symlink に差し替えられる", func(t *testing.T) {
		tr, dir := setup(t)
		tr.hook = func(stage, name string) {
			if stage == stageAfterOpen && name == "docs/reference/core.md" {
				p := filepath.Join(dir, "docs", "reference", "core.md")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				symlink(t, "../../README.md", p)
			}
		}
		if err := tr.writeFile("docs/reference/core.md", []byte("生成物\n")); err == nil {
			t.Error("error にすべき")
		}
		unchanged(t, dir)
	})

	t.Run("親のディレクトリが symlink", func(t *testing.T) {
		tr, dir := setup(t)
		if err := os.RemoveAll(filepath.Join(dir, "docs")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
			t.Fatal(err)
		}
		symlink(t, "elsewhere", filepath.Join(dir, "docs"))
		if err := tr.writeFile("docs/reference/core.md", []byte("生成物\n")); err == nil {
			t.Error("error にすべき (親が symlink)")
		}
		if _, err := os.Lstat(filepath.Join(dir, "elsewhere", "reference")); err == nil {
			t.Error("symlink の先に、ディレクトリができた")
		}
	})

	t.Run("FIFO", func(t *testing.T) {
		tr, dir := setup(t)
		mkfifo(t, filepath.Join(dir, "docs", "reference", "core.md"))
		within(t, 5*time.Second, func() {
			if err := tr.writeFile("docs/reference/core.md", []byte("生成物\n")); err == nil {
				t.Error("error にすべき")
			}
		})
	})

	t.Run("対照: 通常のファイルには書ける", func(t *testing.T) {
		tr, dir := setup(t)
		if err := tr.writeFile("docs/reference/keep.md", []byte("新\n")); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "docs", "reference", "keep.md")); string(b) != "新\n" {
			t.Errorf("中身 = %q", b)
		}
	})
}

// TestListReferenceRejectsNonRegular は、生成物の置き場の中の symlink・FIFO が、開かれずに error になることを確認する。
func TestListReferenceRejectsNonRegular(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"docs/reference/a.md": "x\n", "README.md": "x\n"})
		symlink(t, "../../README.md", filepath.Join(dir, "docs", "reference", "link.md"))
		if got, err := tr.listReference(); err == nil {
			t.Errorf("error にすべき: %+v", got)
		}
	})
	t.Run("FIFO", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"docs/reference/a.md": "x\n"})
		mkfifo(t, filepath.Join(dir, "docs", "reference", "fifo.md"))
		within(t, 5*time.Second, func() {
			if got, err := tr.listReference(); err == nil {
				t.Errorf("error にすべき: %+v", got)
			}
		})
	})
	t.Run("docs が symlink", func(t *testing.T) {
		tr, dir := newTestTree(t, map[string]string{"real/reference/a.md": "x\n"})
		symlink(t, "real", filepath.Join(dir, "docs"))
		if got, err := tr.listReference(); err == nil {
			t.Errorf("error にすべき: %+v", got)
		}
	})
}

// TestErrorsAreBudgetOrPlain は、予算超過だけが errBudget で、それ以外の error と区別できることを確認する。
func TestErrorsAreBudgetOrPlain(t *testing.T) {
	tr, dir := newTestTree(t, map[string]string{"core/doc.go": "package core\n"})
	symlink(t, "doc.go", filepath.Join(dir, "core", "link.go"))
	_, err := tr.inventory()
	if err == nil || errors.Is(err, errBudget) {
		t.Errorf("symlink の error は、予算超過ではない: %v", err)
	}
	if !strings.Contains(err.Error(), "core/link.go") {
		t.Errorf("error にファイル名が無い: %v", err)
	}
}

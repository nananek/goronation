package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTree は、root の下に、files (repo 相対 path → 中身) を作る。
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// newTestTree は、一時ディレクトリに files を作り、既定の予算で tree を開く。
func newTestTree(t *testing.T, files map[string]string) (*tree, string) {
	t.Helper()
	return newTestTreeLimits(t, files, defaultLimits)
}

func newTestTreeLimits(t *testing.T, files map[string]string, lim limits) (*tree, string) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)
	return newTree(openTestRoot(t, dir), lim), dir
}

// symlink は、link (絶対 path) に、target を指す symlink を作る。作れない環境では skip する。
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
}

// within は、fn が d 以内に終わることを確かめる。止まる実装 (writer の無い FIFO を開くなど) が、
// テスト全体を道連れにしないため。
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

// fixtureRepo は、repoRoot・openRepo が受け入れる最小の repo (go.work と tools/docgen/go.mod) を作り、その root を返す。
func fixtureRepo(t *testing.T, extra map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.work":             "go 1.24.0\n\nuse ./tools/docgen\n",
		"tools/docgen/go.mod": "module github.com/nananek/goronation/tools/docgen\n\ngo 1.24.0\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	writeTree(t, root, files)
	return root
}

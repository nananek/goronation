package archtest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// findRoot は start から上方向に go.work を探し、それが置かれたディレクトリを返す。
func findRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.work")); err == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.work が見つからない")
		}
		dir = parent
	}
}

func TestFindRoot(t *testing.T) {
	t.Run("見つかる", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			"go.work":         "go 1.24.0\n",
			"a/b/c/keep.txt":  "",
			"a/b/go.work/x.z": "", // go.work という名前のディレクトリは go.work ではない
		})
		got, err := findRoot(filepath.Join(root, "a", "b", "c"))
		if err != nil {
			t.Fatal(err)
		}
		if got != root {
			t.Errorf("findRoot = %q, want %q", got, root)
		}
	})
	t.Run("見つからなければ error", func(t *testing.T) {
		if _, err := findRoot(t.TempDir()); err == nil {
			t.Fatal("error を返すべき")
		}
	})
}

// TestRepository は、このリポジトリ自体が規則を満たすことを確認する。
// archtest 自身も対象で、自己免除しない。
func TestRepository(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := findRoot(wd)
	if err != nil {
		t.Fatalf("repo の root を決められない: %v", err)
	}
	// root を取り違えると、相対パスの規則 (core/** など) が黙って空振りする。
	if _, err := os.Stat(filepath.Join(root, "tools", "archtest", "archtest.go")); err != nil {
		t.Fatalf("%s は repo の root ではない: %v", root, err)
	}

	violations, scanned, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatalf("Check がエラーを返した: %v", err)
	}
	if scanned == 0 {
		t.Fatal("走査した .go が 0 ファイル (空振りで緑にしない)")
	}
	for _, v := range violations {
		t.Errorf("%s", v)
	}
	t.Logf("%d 個の .go を走査した (root=%s)", scanned, root)
}

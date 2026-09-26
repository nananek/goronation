//go:build unix

package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// limitTree は、root に、name から hops 段の symlink の連鎖を作る。各段の target は "a/../a/../...<次の段>" (1560 要素。
// 実在するディレクトリ core/a を出入りするだけ) で、最後は final を指す (通常のファイル core/real.go か、存在しない名前)。
// 連鎖の全体の要素の数は、約 1560 × hops になる (maxResolveSteps は 4096)。
func limitTree(t *testing.T, root, name string, hops int, final string) {
	t.Helper()
	junk := strings.Repeat("a/../", 780) // 3900 バイト。symlink の target の上限 (4095) 以内
	writeTree(t, root, map[string]string{"core/real.go": "package core\n"})
	if err := os.MkdirAll(filepath.Join(root, "core", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.ToSlash(filepath.Dir(name))
	linkAt(t, root, name, "_h1")
	for i := 1; i <= hops; i++ {
		next := fmt.Sprintf("_h%d", i+1)
		if i == hops {
			next = final
		}
		linkAt(t, root, fmt.Sprintf("%s/_h%d", dir, i), junk+next)
	}
}

// TestProcessDependentStepLimit は、processDependent の手数の上限 (maxResolveSteps) を確認する。上限を超える連鎖は、
// 黙って通さず (時間の上限のために打ち切って、通すのではなく)、プロセス依存として扱う (fail-closed)。上限以内の、
// 同じ形の連鎖は、これまでどおり通す。build に使われない名前は、判定しないので、何も言わない。
func TestProcessDependentStepLimit(t *testing.T) {
	t.Run("上限を超える連鎖の .go は、error", func(t *testing.T) {
		root := procTree(t)
		limitTree(t, root, "core/long.go", 3, "real.go") // 約 4700 要素
		wantProcessDependentError(t, root, "core/long.go")
		_, _, err := Check(root, DefaultRules)
		if !strings.Contains(err.Error(), "上限") {
			t.Errorf("error が、上限を超えたことを示さない: %v", err)
		}
	})

	t.Run("上限以内の、同じ形の連鎖の .go は、通す", func(t *testing.T) {
		root := procTree(t)
		limitTree(t, root, "core/short.go", 2, "real.go") // 約 3100 要素
		vs, _, err := Check(root, DefaultRules)
		if err != nil || len(vs) != 0 {
			t.Errorf("通すべき: err=%v violations=%v", err, keys(vs))
		}
	})

	// 連鎖の最後は存在しない名前 (壊れた symlink)。上限が無いと、modules.txt は無いものとして、違反にならない。
	t.Run("上限を超える連鎖の vendor/modules.txt は、違反", func(t *testing.T) {
		root := procTree(t)
		limitTree(t, root, "vendor/modules.txt", 3, "_end")
		vs, _, err := Check(root, DefaultRules)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"vendor/modules.txt:1: vendor-mode"}; !slices.Equal(keys(vs), want) {
			t.Errorf("violations=%v (want %v)", keys(vs), want)
		}
	})

	t.Run("上限以内の連鎖の vendor/modules.txt (壊れた symlink) は、違反にしない", func(t *testing.T) {
		root := procTree(t)
		limitTree(t, root, "vendor/modules.txt", 2, "_end")
		vs, _, err := Check(root, DefaultRules)
		if err != nil || len(vs) != 0 {
			t.Errorf("通すべき: err=%v violations=%v", err, keys(vs))
		}
	})

	t.Run("build に使われない名前は、上限を超えても、何も言わない", func(t *testing.T) {
		root := procTree(t)
		limitTree(t, root, "core/long.txt", 3, "real.go")
		vs, _, err := Check(root, DefaultRules)
		if err != nil || len(vs) != 0 {
			t.Errorf("通すべき: err=%v violations=%v", err, keys(vs))
		}
	})
}

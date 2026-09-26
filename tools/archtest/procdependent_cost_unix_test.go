//go:build unix

package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProcessDependentCost は、build に使われない名前の symlink を、多数 (連鎖で) 置いても、Check の時間が増えないことを確認する。
//
// processDependent は、symlink の連鎖を 1 段ずつ辿り、途中の path の要素ごとに Lstat する。1 回の判定の上限は、連鎖 255 段 × target の
// 要素数 (target は 4095 バイトまで) で、1 本で約 0.4 秒かかる。scan は、この判定を、build に使われない名前 (_h1 など。判定の結果を
// 使わない) の symlink を含む、全部の symlink に対して行う。連鎖の各段も、それぞれ symlink として判定されるため、255 本の連鎖だけで
// (実測) 52 秒、連鎖を指す symlink を 200 本足すと 2 分 14 秒かかる。判定の結果を使う名前 (.go・go.mod・go.work) と vendor に絞れば、
// この増幅は無い (修正前の bb58a1a と同じ、1 秒未満)。build に使われない名前の symlink に、判定の時間を払わせてはいけない。
//
// このテストはコードを実行しない。連鎖の各段の target は "a/../a/../...<次の段の名前>" で、実在するディレクトリ a を出入りするだけ。
// 最後の段は存在しない名前を指す (壊れた symlink。build に使われない名前なので、Check は error にも違反にもしない)。
func TestProcessDependentCost(t *testing.T) {
	const (
		hops  = 255 // 連鎖の段数
		heads = 100 // 連鎖の先頭を指す symlink の本数 (1 本ごとに、判定に約 0.4 秒かかる。速い環境でも、10 秒に収まらないようにする余裕)
	)
	junk := strings.Repeat("a/../", 780) // 3900 バイト。symlink の target の上限 (4095) 以内
	root := procTree(t)
	if err := os.MkdirAll(filepath.Join(root, "core", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= hops; i++ {
		next := fmt.Sprintf("_h%d", i+1)
		if i == hops {
			next = "_end"
		}
		linkAt(t, root, fmt.Sprintf("core/_h%d", i), junk+next)
	}

	for i := 0; i < heads; i++ {
		linkAt(t, root, fmt.Sprintf("core/_head%d", i), "_h1")
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Check(root, DefaultRules)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Check が 10 秒で終わらない (build に使われない名前の symlink %d 本。修正前の bb58a1a では、同じ形の 455 本で 2 秒)", hops+heads)
	}
	t.Logf("Check: %v (symlink %d 本)", time.Since(start).Round(time.Millisecond), hops+heads)
}

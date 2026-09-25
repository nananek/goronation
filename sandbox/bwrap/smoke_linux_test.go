package bwrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	// bwrapPath は固定パスの起動器。PATH は検索しない。
	bwrapPath = "/usr/bin/bwrap"

	// requireEnv が "1" のとき、bwrap を使えなければ skip ではなく fail する。
	// 実行層の CI leg が、全部 skip されたまま緑になるのを防ぐ。
	requireEnv = "GORO_REQUIRE_BWRAP"

	smokeTimeout = 10 * time.Second

	// waitDelay は、smokeTimeout で kill したあと、出力の pipe を待つ猶予。
	// bwrap の子孫が pipe を握ったまま残ると、CombinedOutput はそれが閉じるまで
	// 戻らず、timeout が実際には効かない (WaitDelay が無いと 10 秒が 30 秒になる)。
	waitDelay = time.Second
)

// TestSmoke は、bwrap が起動して、檻の中で /bin/true を実行できることだけを確認する。
// ネットワーク遮断などの中身の検証は、後続の spike (S2) で行う。引数は定数だけで作る。
func TestSmoke(t *testing.T) {
	// unavailable は「bwrap を使えない」ときの終わり方を決める。
	unavailable := func(format string, args ...any) {
		t.Helper()
		reason := fmt.Sprintf(format, args...)
		if os.Getenv(requireEnv) == "1" {
			t.Fatalf("%s=1 だが bwrap を使えない: %s", requireEnv, reason)
		}
		t.Skipf("bwrap を使えないため skip する (%s=1 で必須になる): %s", requireEnv, reason)
	}

	if _, err := os.Stat(bwrapPath); err != nil {
		unavailable("%s が無い: %v", bwrapPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()

	// run は bwrap を実行し、出力を返す。timeout は使えないのではなく異常なので、常に fail にする。
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bwrapPath, args...)
		cmd.WaitDelay = waitDelay
		out, err := cmd.CombinedOutput()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("bwrap %v が %s で終わらない: %s", args, smokeTimeout, out)
		}
		return out, err
	}

	// Ubuntu 24.04 などの版差の記録用。
	if out, err := run("--version"); err != nil {
		unavailable("--version が失敗した: %v: %s", err, out)
	} else {
		t.Logf("%s", strings.TrimSpace(string(out)))
	}

	out, err := run(
		"--unshare-all",
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--", "/bin/true",
	)
	if err != nil {
		unavailable("檻の中で /bin/true を実行できない: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

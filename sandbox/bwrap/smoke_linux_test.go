package bwrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// requireEnv が "1" のとき、bwrap を使えなければ skip ではなく fail する。
	// 実行層の CI leg が、全部 skip されたまま緑になるのを防ぐ。
	requireEnv = "GORO_REQUIRE_BWRAP"

	smokeTimeout = 10 * time.Second

	// smokeWaitDelay は、smokeTimeout で kill したあと、出力の pipe を待つ猶予。
	// bwrap の子孫が pipe を握ったまま残ると、CombinedOutput はそれが閉じるまで
	// 戻らず、timeout が実際には効かない (WaitDelay が無いと、待ち時間に上限が無い)。
	smokeWaitDelay = time.Second
)

// bwrapProbe は、bwrap を使えるかの確認結果 (プロセスの中で 1 回だけ確かめる)。
var bwrapProbe struct {
	once    sync.Once
	version string
	reason  string // 空なら使える
}

// needBwrap は、bwrap を使えなければ、テストを skip する。requireEnv が "1" なら、skip ではなく fail する。
// bwrap を使えるとは、固定パスに在り、檻の中で /bin/true を実行できること (この確認だけは、Spec を使わず、引数を定数で作る)。
func needBwrap(t *testing.T) {
	t.Helper()
	bwrapProbe.once.Do(func() {
		if _, err := os.Stat(bwrapPath); err != nil {
			bwrapProbe.reason = fmt.Sprintf("%s が無い: %v", bwrapPath, err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
		defer cancel()
		// run は bwrap を実行し、出力を返す。
		run := func(args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, bwrapPath, args...)
			cmd.WaitDelay = smokeWaitDelay
			out, err := cmd.CombinedOutput()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return out, fmt.Errorf("%s で終わらない", smokeTimeout)
			}
			return out, err
		}
		// Ubuntu 24.04 などの版差の記録用。
		out, err := run("--version")
		if err != nil {
			bwrapProbe.reason = fmt.Sprintf("--version が失敗した: %v: %s", err, out)
			return
		}
		bwrapProbe.version = strings.TrimSpace(string(out))
		if out, err := run(
			"--unshare-all",
			"--die-with-parent",
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
			"--", "/bin/true",
		); err != nil {
			bwrapProbe.reason = fmt.Sprintf("檻の中で /bin/true を実行できない: %v: %s", err, strings.TrimSpace(string(out)))
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

// TestSmoke は、bwrap が起動して、檻の中で /bin/true を実行できることだけを確認する。
// 引数は定数だけで作る (Spec は使わない。--ro-bind / / は、この smoke 専用で、Spec からは出せない)。
func TestSmoke(t *testing.T) {
	needBwrap(t)
	t.Log(bwrapProbe.version)
}

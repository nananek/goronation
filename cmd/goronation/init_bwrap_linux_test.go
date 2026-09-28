package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	// bwrapPath は固定パスの起動器。PATH は検索しない。
	bwrapPath = "/usr/bin/bwrap"

	// requireBwrapEnv が "1" のとき、bwrap を使えなければ skip ではなく fail する
	// (CI の bwrap leg が、全部 skip されたまま緑になるのを防ぐ)。
	requireBwrapEnv = "GORO_REQUIRE_BWRAP"

	// 檻の中の path。
	jailExe  = "/goro-test"
	jailSock = "/run/goro/up.sock"
)

func init() {
	helpers["jailreap"] = helperJailReap
	helpers["orphan-maker"] = helperOrphanMaker
	helpers["sleep-short"] = func([]string) int { time.Sleep(200 * time.Millisecond); return 0 }
}

// requireBwrap は、bwrap を使えなければ、skip (GORO_REQUIRE_BWRAP=1 なら fail) する。
func requireBwrap(t *testing.T) {
	t.Helper()
	unavailable := func(format string, args ...any) {
		t.Helper()
		reason := fmt.Sprintf(format, args...)
		if os.Getenv(requireBwrapEnv) == "1" {
			t.Fatalf("%s=1 だが bwrap を使えない: %s", requireBwrapEnv, reason)
		}
		t.Skipf("bwrap を使えないため skip する (%s=1 で必須になる): %s", requireBwrapEnv, reason)
	}
	if _, err := os.Stat(bwrapPath); err != nil {
		unavailable("%s が無い: %v", bwrapPath, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrapPath, "--unshare-all", "--die-with-parent",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--", "/bin/true")
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		unavailable("檻の中で /bin/true を実行できない: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// runInJail は、--unshare-all の檻の中で、goro init を動かし、子として selfCmd の helper を実行する。
// 檻に置くのは、ホストの /usr (テストバイナリの動的リンクの分)・テストバイナリ・上流の UDS (ro) だけ。
// asPID1 なら、goro init が檻の PID 1 になる (bwrap の --as-pid-1)。
func runInJail(t *testing.T, sock string, asPID1 bool, helper string) (string, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "GORACE", "atexit_sleep_ms=0", // 檻の環境変数は空。-race の終了時の 1 秒待ちを避ける
	}
	if asPID1 {
		args = append(args, "--as-pid-1")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--ro-bind", "/usr", "/usr")
	for _, dir := range []string{"/bin", "/sbin", "/lib", "/lib64"} { // 多くの配布は /usr への symlink
		if _, err := os.Stat(dir); err == nil {
			args = append(args, "--ro-bind", dir, dir)
		}
	}
	args = append(args, "--ro-bind", exe, jailExe, "--ro-bind", sock, jailSock)
	args = append(args, "--", jailExe, helperArg, "goro", "init",
		"--listen", "127.0.0.1:0", "--upstream", jailSock, "--", jailExe, helperArg, helper)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrapPath, args...)
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("檻が %s で終わらない: %s", testTimeout, out.String())
	}
	if cmd.ProcessState == nil {
		t.Fatalf("bwrap を実行できない: %v", err)
	}
	return out.String(), cmd.ProcessState.ExitCode()
}

// 檻の中の子が、環境変数のアドレスに繋ぐだけで、ro で bind した UDS 越しに、ホストの上流へ届く。
func TestInitInBwrapRelay(t *testing.T) {
	requireBwrap(t)
	sock := tempSock(t)
	seen := make(chan string, 1)
	startUpstream(t, sock, func(c net.Conn) {
		defer c.Close()
		b, _ := io.ReadAll(c)
		seen <- string(b)
		c.Write(append([]byte(hostSawPrefix), b...))
	})

	out, code := runInJail(t, sock, false, "proxyclient")
	if code != 0 || out != hostSawPrefix+"ping" {
		t.Fatalf("檻の中の子: 終了コード %d, 出力 %q (want 0, %q)", code, out, hostSawPrefix+"ping")
	}
	select {
	case got := <-seen:
		if got != "ping" {
			t.Errorf("ホストの上流が受けたバイト列 = %q, want ping", got)
		}
	default:
		t.Error("ホストの上流に、何も届いていない")
	}
}

// goro init が檻の PID 1 のとき、孤児 (子の子が、親より先に親を失ったもの) を回収する。
func TestInitInBwrapReapsOrphans(t *testing.T) {
	requireBwrap(t)
	// 上流は使わないが、bind する UDS が要る。
	sock := tempSock(t)
	startUpstream(t, sock, echo)

	out, code := runInJail(t, sock, true, "jailreap")
	if code != 0 {
		t.Fatalf("終了コード = %d (7: zombie が残った, 9: goro init が PID 1 ではない)\n%s", code, out)
	}
}

// helperJailReap は、檻の中で、孤児を作り、zombie が残らないことを確かめる (残れば 7、前提が崩れれば 9)。
func helperJailReap([]string) int {
	if ppid := os.Getppid(); ppid != 1 {
		fmt.Printf("親が PID 1 ではない (ppid=%d)\n", ppid)
		return 9
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Println(err)
		return 1
	}
	// 中間の子が、孫を起動して、待たずに終わる。孫は孤児になり、PID 1 (goro init) に引き取られる。
	if err := exec.Command(exe, helperArg, "orphan-maker").Run(); err != nil {
		fmt.Println(err)
		return 1
	}
	time.Sleep(700 * time.Millisecond) // 孫が終わるのを待つ (sleep-short は 200ms)
	if z := zombies(); len(z) > 0 {
		fmt.Printf("zombie が残っている: %q\n", z)
		return 7
	}
	return 0
}

// helperOrphanMaker は、孫を起動し、待たずに終わる。
func helperOrphanMaker([]string) int {
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	if err := exec.Command(exe, helperArg, "sleep-short").Start(); err != nil {
		return 1
	}
	return 0
}

// zombies は、/proc から、zombie (状態 Z) のプロセスの stat を集める。
func zombies() []string {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, e := range ents {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // 読む間に終わった
		}
		s := string(b)
		// "pid (comm) S ..."。comm に ')' を含みうるので、最後の ')' から数える。
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) && s[i+2] == 'Z' {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

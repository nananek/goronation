//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	helpers["dumpable-check"] = helperDumpableCheck
	helpers["ptrace-selftest"] = helperPtraceSelfTest
	helpers["ptrace-attacker"] = helperPtraceAttacker
}

// prGetDumpable は、Linux の <sys/prctl.h> の PR_GET_DUMPABLE。
const prGetDumpable = 3

// getDumpable は、自分の dumpable (prctl PR_GET_DUMPABLE) を返す。
func getDumpable() (int, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_PRCTL, uintptr(prGetDumpable), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// helperDumpableCheck は、自分の dumpable を出す (goronation init が、__exec-hardened 経由で子を起動していること
// (TestInitHardensChildDumpable) の確認に使う)。
func helperDumpableCheck([]string) int {
	v, err := getDumpable()
	if err != nil {
		fmt.Println("err:", err)
		return 1
	}
	fmt.Println("dumpable:", v)
	return 0
}

// TestInitDoesNotActuallyHardenChildDumpable は、goronation init が、fork した子を __exec-hardened 経由で
// 起動しても (setDumpableOff を、fork 直後・execve 前に正しく呼んでいても)、成り代わった先の子自身の dumpable は
// 0 のままにはならず、1 に戻ることを確認する。
//
// 既知の限界 (要検討・未解決): Linux は、特権の変わらない通常の execve では、dumpable を都度 1 に戻す
// (prctl(2) の PR_SET_DUMPABLE の項。setuid/setgid や file capabilities で特権が変わる execve のときだけ、
// suid_dumpable の値になる。claude・opencode のような通常の実行ファイルは、そのどれにも当たらない)。そのため、
// __exec-hardened が自分に dumpable=0 を設定してから execve しても、成り代わったエージェント本体には引き継がれない
// (exechardened.go の execHardenedUsage・cage.go の defaultCapDrop のコメント・doc.go の「限界」に既知の限界として
// 明記した)。同一檻内の兄弟プロセスからの ptrace・procfs 経由の介入 (TestExecHardenedDumpableBlocksSiblingPtrace の
// 「dumpable=1」の場合と同じ状況) を、実際に防ぐには、この限界の解消 (別の機構・別の設計) が要る。
func TestInitDoesNotActuallyHardenChildDumpable(t *testing.T) {
	cmd := initCmd(t, "127.0.0.1:0", "/nonexistent/up.sock", nil, selfCmd(t, "dumpable-check")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "dumpable: 1" {
		t.Errorf("子の dumpable = %q, want %q (execve が dumpable を 1 に戻さなくなった: この既知の限界が解消したなら、"+
			"__exec-hardened の設計とドキュメントを見直す)", got, "dumpable: 1")
	}
}

// helperPtraceSelfTest は、args[0] ("0"/"1") が "1" なら、自分に dumpable=0 を設定してから、自分の pid を渡した
// ptrace-attacker を子として起こし、その終了を待つ。
func helperPtraceSelfTest(args []string) int {
	if len(args) > 0 && args[0] == "1" {
		if err := setDumpableOff(); err != nil {
			fmt.Println("dumpable-err:", err)
			return 1
		}
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("exe-err:", err)
		return 1
	}
	cmd := exec.Command(exe, helperArg, "ptrace-attacker", strconv.Itoa(os.Getpid()))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Println("attacker-run-err:", err)
		return 1
	}
	return 0
}

// helperPtraceAttacker は、args[0] の pid (自分の兄弟にあたる、ptrace-selftest の役) に、procfs (/proc/<pid>/fd/1)
// と ptrace (PTRACE_ATTACH) で届くかを試す。PTRACE_ATTACH が成功したら、必ず wait してから detach する
// (attach は対象を SIGSTOP で止める。wait でその通知を消費しないまま放置すると、対象が止まったまま残る)。
func helperPtraceAttacker(args []string) int {
	if len(args) == 0 {
		fmt.Println("no-pid")
		return 1
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Println("bad-pid:", err)
		return 1
	}
	if _, ferr := os.Open(fmt.Sprintf("/proc/%d/fd/1", pid)); ferr != nil {
		fmt.Println("fd-open: blocked:", ferr)
	} else {
		fmt.Println("fd-open: ok")
	}
	if perr := syscall.PtraceAttach(pid); perr != nil {
		fmt.Println("ptrace-attach: blocked:", perr)
	} else {
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
		_ = syscall.PtraceDetach(pid)
		fmt.Println("ptrace-attach: ok")
	}
	return 0
}

// runPtraceJail は、bwrap の --cap-drop CAP_SYS_PTRACE つきの檻の中で、ptrace-selftest (dumpableArg) を起こし、
// その出力 (fd-open・ptrace-attach の結果) を返す。init_bwrap_linux_test.go の requireBwrap・bwrapPath・jailExe を使う。
func runPtraceJail(t *testing.T, dumpableArg string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "GORACE", "atexit_sleep_ms=0", // 檻の環境変数は空。-race の終了時の 1 秒待ちを避ける
		"--cap-drop", "CAP_SYS_PTRACE",
		"--proc", "/proc", "--dev", "/dev", "--ro-bind", "/usr", "/usr",
	}
	for _, dir := range []string{"/bin", "/sbin", "/lib", "/lib64"} { // 多くの配布は /usr への symlink
		if _, err := os.Stat(dir); err == nil {
			args = append(args, "--ro-bind", dir, dir)
		}
	}
	args = append(args, "--ro-bind", exe, jailExe)
	args = append(args, "--", jailExe, helperArg, "ptrace-selftest", dumpableArg)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrapPath, args...)
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("檻が %s で終わらない: %s", testTimeout, out.String())
	}
	return out.String()
}

// TestExecHardenedDumpableBlocksSiblingPtrace は、seccomp-capdrop-plan の技術的発見 (cap-drop だけでは、Yama LSM の
// 既定 (ptrace_scope=0)・同一 uid・dumpable のプロセス間の ptrace/procfs アクセスを防げない) を、実 bwrap で確かめる。
// CAP_SYS_PTRACE を落とした檻の中でも、対象プロセスが dumpable=1 (既定) なら、兄弟プロセスは procfs・ptrace で届く
// (赤: この檻の前提そのものが崩れていないことの確認でもある)。対象プロセスが、setDumpableOff (goronation
// __exec-hardened と同じ prctl(PR_SET_DUMPABLE, 0)) を自分に設定し、以後 execve しなければ、届かない (緑)。
//
// 注意: これは、prctl 自体の効果の確認であり、goronation init の実際の経路 (__exec-hardened が dumpable=0 を
// 設定してから、エージェント本体へ execve する) がエージェント本体を守れることの確認ではない。その経路は、
// execve が dumpable を 1 に戻すため、実際には守れない (TestInitDoesNotActuallyHardenChildDumpable が確認する、
// 既知の限界)。
func TestExecHardenedDumpableBlocksSiblingPtrace(t *testing.T) {
	requireBwrap(t)

	t.Run("dumpable=1 (既定): cap-drop だけでは防げない", func(t *testing.T) {
		out := runPtraceJail(t, "0")
		if !strings.Contains(out, "fd-open: ok") {
			t.Errorf("dumpable=1 なのに、兄弟プロセスが /proc/<pid>/fd/1 を開けない (この檻の前提が崩れている): %s", out)
		}
		if !strings.Contains(out, "ptrace-attach: ok") {
			t.Errorf("dumpable=1 なのに、兄弟プロセスの ptrace-attach が通らない (この檻の前提が崩れている): %s", out)
		}
	})

	t.Run("dumpable=0 (goronation __exec-hardened と同じ)", func(t *testing.T) {
		out := runPtraceJail(t, "1")
		if strings.Contains(out, "fd-open: ok") {
			t.Errorf("dumpable=0 なのに、兄弟プロセスが /proc/<pid>/fd/1 を開けた: %s", out)
		}
		if strings.Contains(out, "ptrace-attach: ok") {
			t.Errorf("dumpable=0 なのに、兄弟プロセスが ptrace-attach できた: %s", out)
		}
	})
}

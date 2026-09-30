//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

const landlockExecUsage = `使い方: goronation landlock-exec --allow-connect PORT[,PORT...] -- EXE [ARGS...]

EXE (絶対 path) を、Landlock (TCP の connect を、許可したポートだけにする) と seccomp (Landlock が止めない MPTCP・SCTP・UDP など TCP 以外の inet の socket・MSG_FASTOPEN・io_uring を
EPERM にする) と no_new_privs を掛けてから、exec する (ADR 0020)。制限は EXE とその子孫に掛かる。どれか 1 つでも掛けられなければ、EXE を起動せず、
理由を言って終わる (fail closed)。Landlock の ABI が 4 (Linux 6.7) 未満・x86_64 以外では起動しない。

  --allow-connect PORT[,PORT...]   connect を許す TCP のポート (1 つ以上。egress の proxy のポート)
`

// Landlock・seccomp の定数 (x86_64 の syscall 番号。ほかの arch は、実行時に断る)。
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1 << 0
	landlockAccessNetConnectTCP  = 1 << 1
	landlockRuleNetPort          = 2
	landlockMinABI               = 4

	prSetNoNewPrivs   = 38
	prGetNoNewPrivs   = 39
	prSetSeccomp      = 22
	seccompModeFilter = 2
)

// lockdown は、制限を掛ける手順の差し込み口 (テストが、失敗を模す)。どれかが失敗したら、exec は呼ばれない。
type lockdown struct {
	arch       func() string
	abi        func() (int, error)
	noNewPrivs func() error
	landlock   func(ports []int) error
	seccomp    func() error
	exec       func(path string, argv, env []string) error
}

func realLockdown() lockdown {
	return lockdown{
		arch:       func() string { return runtime.GOARCH },
		abi:        landlockABI,
		noNewPrivs: setNoNewPrivs,
		landlock:   landlockRestrictConnect,
		seccomp:    installSeccomp,
		exec:       syscall.Exec,
	}
}

// lockdownError は、制限を掛けられなかったこと (EXE は動いていない)。exec の失敗とは区別する。
type lockdownError struct{ err error }

func (e *lockdownError) Error() string { return e.err.Error() }
func (e *lockdownError) Unwrap() error { return e.err }

// apply は、制限を掛けて exec する。exec に成功すれば戻らない。戻ったら、必ず error (掛けられなかった理由・exec の失敗)。
// 手順の順: arch・ABI の検査 → no_new_privs → Landlock → seccomp → exec。どこかで失敗したら、その先に進まない。
func (l lockdown) apply(ports []int, exe string, argv, env []string) error {
	if err := l.restrict(ports); err != nil {
		return &lockdownError{err}
	}
	return l.exec(exe, argv, env)
}

// restrict は、制限を掛ける手順 (exec の前まで)。
func (l lockdown) restrict(ports []int) error {
	if a := l.arch(); a != "amd64" {
		return fmt.Errorf("x86_64 (amd64) 以外 (%s) では起動しない (seccomp の syscall 番号と arch が無い)", a)
	}
	abi, err := l.abi()
	if err != nil {
		return fmt.Errorf("Landlock を使えない: %w", err)
	}
	if abi < landlockMinABI {
		return fmt.Errorf("Landlock の ABI が %d (TCP の connect の制限は %d 以上が要る。Linux 6.7 以降)", abi, landlockMinABI)
	}
	if err := l.noNewPrivs(); err != nil {
		return fmt.Errorf("no_new_privs を立てられない: %w", err)
	}
	if err := l.landlock(ports); err != nil {
		return fmt.Errorf("Landlock の制限を掛けられない: %w", err)
	}
	if err := l.seccomp(); err != nil {
		return fmt.Errorf("seccomp の制限を掛けられない: %w", err)
	}
	return nil
}

// runLandlockExec は、goronation landlock-exec の本体。
func runLandlockExec(args []string, stderr io.Writer) int {
	return runLandlockExecWith(args, stderr, realLockdown())
}

func runLandlockExecWith(args []string, stderr io.Writer, l lockdown) int {
	// Landlock・seccomp は、呼んだスレッドだけに効く (TSYNC は使わない)。exec も同じスレッドから行うので、このゴルーチンを固定する
	// (Unlock しない: exec するか、終わる)。
	runtime.LockOSThread()

	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "goronation landlock-exec: "+format+"\n", a...)
		return code
	}
	flags := flag.NewFlagSet("landlock-exec", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	allow := flags.String("allow-connect", "", "")
	if err := flags.Parse(args); err != nil {
		fmt.Fprint(stderr, landlockExecUsage)
		return fail(exitUsage, "引数が不正: %v", err)
	}
	ports, err := parsePorts(*allow)
	if err != nil {
		return fail(exitUsage, "%v\n%s", err, landlockExecUsage)
	}
	argv := flags.Args()
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return fail(exitUsage, "起動する実行ファイルは、絶対 path で 1 つ要る\n%s", landlockExecUsage)
	}
	err = l.apply(ports, argv[0], argv, os.Environ())
	// apply が戻ったのは、制限を掛けられなかったか、exec に失敗したか。どちらも、EXE は動いていない。
	var le *lockdownError
	switch {
	case errors.As(err, &le):
		return fail(exitLockdown, "%v", err)
	case errors.Is(err, syscall.ENOENT):
		return fail(exitNotFound, "%v", err)
	}
	return fail(exitNoExec, "起動できない: %v", err)
}

// landlockABI は、kernel の Landlock の ABI の版を返す。使えなければ error (ENOSYS: 無い、EOPNOTSUPP: 無効)。
func landlockABI() (int, error) {
	v, _, e := syscall.RawSyscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if e != 0 {
		return 0, e
	}
	return int(v), nil
}

// landlockRestrictConnect は、自分 (呼んだスレッド) の TCP の connect を、ports だけにする。bind は制限しない (opencode は自分のポートを待ち受ける)。
func landlockRestrictConnect(ports []int) error {
	attr := [2]uint64{0, landlockAccessNetConnectTCP} // handled_access_fs・handled_access_net
	fd, _, e := syscall.RawSyscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	runtime.KeepAlive(&attr)
	if e != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	defer syscall.Close(int(fd))
	for _, p := range ports {
		rule := [2]uint64{landlockAccessNetConnectTCP, uint64(p)} // packed の {allowed_access, port}
		if _, _, e := syscall.RawSyscall6(sysLandlockAddRule, fd, landlockRuleNetPort, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); e != 0 {
			return fmt.Errorf("landlock_add_rule (port %d): %w", p, e)
		}
		runtime.KeepAlive(&rule)
	}
	if _, _, e := syscall.RawSyscall(sysLandlockRestrictSelf, fd, 0, 0); e != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", e)
	}
	return nil
}

func setNoNewPrivs() error {
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return e
	}
	// 立ったことを確かめる。
	if v, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0, 0, 0, 0); e != 0 || v != 1 {
		return fmt.Errorf("PR_GET_NO_NEW_PRIVS = %d (%v)", v, e)
	}
	return nil
}

func installSeccomp() error {
	prog, err := seccompProgram()
	if err != nil {
		return err
	}
	fprog := syscall.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&fprog)), 0, 0, 0); e != 0 {
		return e
	}
	runtime.KeepAlive(prog)
	return nil
}

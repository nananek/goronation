//go:build linux

package contract

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
)

// closeRangeCloexec は、close_range(2) の CLOSE_RANGE_CLOEXEC (Linux 5.11 以降)。範囲の fd を、閉じずに close-on-exec にする。
const closeRangeCloexec = 4

// closeRangeNR は、close_range の syscall 番号。標準の syscall パッケージは、loong64 にしか持たない。
// 統一の番号表を使う arch は、どれも 436。mips は番号が違うので、使わない (呼び手は /proc を辿る)。テストが差し替える。
var closeRangeNR = func() (uintptr, bool) {
	switch runtime.GOARCH {
	case "386", "amd64", "arm", "arm64", "loong64", "ppc64", "ppc64le", "riscv64", "s390x":
		return 436, true
	}
	return 0, false
}

// closeOnExecFrom は、first 以降のすべての fd を close-on-exec にする (fd は閉じない。exec で閉じる)。
// Go の os/exec は、Stdin・Stdout・Stderr・ExtraFiles 以外を閉じないので、呼び手が継承した (CLOEXEC でない) fd は、
// そのまま檻のコマンドに届きうる。それを止める。呼び手のプロセス全体に効く。できなければ error (fail-closed)。
// まず close_range を使い、どんな理由で失敗しても (5.9 未満は ENOSYS、5.9〜5.10 は flag が無く EINVAL、seccomp は EPERM)、/proc/self/fd を辿る。
func closeOnExecFrom(first int) error {
	if nr, ok := closeRangeNR(); ok {
		if _, _, e := syscall.Syscall(nr, uintptr(first), ^uintptr(0), closeRangeCloexec); e == 0 {
			return nil
		}
	}
	return procCloseOnExec(first)
}

// procSelfFd は、自分の fd の一覧。テストが差し替える。
var procSelfFd = "/proc/self/fd"

// procCloseOnExec は、/proc/self/fd を辿って、first 以降の fd を close-on-exec にする。辿れなければ error (fail-closed)。
func procCloseOnExec(first int) error {
	ents, err := os.ReadDir(procSelfFd)
	if err != nil {
		return fmt.Errorf("%s を辿れない: %w", procSelfFd, err)
	}
	for _, e := range ents {
		// 数字でない名前は無い。ReadDir が開いた fd は、もう閉じていて、EBADF で何も起きない。
		if fd, err := strconv.Atoi(e.Name()); err == nil && fd >= first {
			syscall.CloseOnExec(fd)
		}
	}
	return nil
}

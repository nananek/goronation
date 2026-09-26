//go:build unix && !linux

package contract

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// devFd は、自分の fd の一覧 (macOS・BSD の /dev/fd)。テストが差し替える。
var devFd = "/dev/fd"

// closeOnExecFrom は、first 以降のすべての fd を close-on-exec にする (fd は閉じない。exec で閉じる)。
// /dev/fd を辿る。辿れなければ error (fail-closed)。呼び手のプロセス全体に効く。
func closeOnExecFrom(first int) error {
	ents, err := os.ReadDir(devFd)
	if err != nil {
		return fmt.Errorf("%s を辿れない: %w", devFd, err)
	}
	for _, e := range ents {
		// ReadDir が開いた fd は、もう閉じていて、EBADF で何も起きない。
		if fd, err := strconv.Atoi(e.Name()); err == nil && fd >= first {
			syscall.CloseOnExec(fd)
		}
	}
	return nil
}

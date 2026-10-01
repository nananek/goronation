//go:build unix

package eventlog

import (
	"errors"
	"os"
	"syscall"
)

// oNoFollow は、最後の要素が symlink なら開かない (os.Root の中でも、念のため付ける)。
const oNoFollow = syscall.O_NOFOLLOW

// tryLock は、f に排他の flock を、待たずに掛ける。すでに他の fd (自分の別の fd を含む) が持っていれば errLocked。
func tryLock(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lerr error
	if err := rc.Control(func(fd uintptr) {
		for {
			lerr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
			if lerr != syscall.EINTR {
				return
			}
		}
	}); err != nil {
		return err
	}
	if errors.Is(lerr, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return lerr
}

// ownedByMe は、fi が自分の uid のものか。
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

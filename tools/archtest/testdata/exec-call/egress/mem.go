package egress

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func mem() {
	syscall.Mmap(-1, 0, 4096, 0, 0)
	syscall.Mprotect(nil, 0)
	unix.Mmap(-1, 0, 4096, 0, 0)
	unix.Mprotect(nil, 0)
}

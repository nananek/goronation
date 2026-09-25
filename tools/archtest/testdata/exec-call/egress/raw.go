package egress

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func run() {
	syscall.Syscall(0, 0, 0, 0)
	syscall.Syscall6(0, 0, 0, 0, 0, 0, 0)
	syscall.RawSyscall(0, 0, 0, 0)
	syscall.RawSyscall6(0, 0, 0, 0, 0, 0, 0)
	unix.Syscall(0, 0, 0, 0)
	unix.Syscall6(0, 0, 0, 0, 0, 0, 0)
	unix.RawSyscall(0, 0, 0, 0)
	unix.RawSyscall6(0, 0, 0, 0, 0, 0, 0)
	syscall.AllThreadsSyscall(0, 0, 0, 0)
	syscall.AllThreadsSyscall6(0, 0, 0, 0, 0, 0, 0)
	syscall.Syscall9(0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
}

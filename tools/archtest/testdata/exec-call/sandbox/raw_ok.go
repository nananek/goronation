package sandbox

import "syscall"

func raw() {
	syscall.RawSyscall(0, 0, 0, 0)
}

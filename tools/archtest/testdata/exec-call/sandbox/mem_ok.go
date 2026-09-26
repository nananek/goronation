package sandbox

import "syscall"

func mem() {
	syscall.Mmap(-1, 0, 4096, 0, 0)
	syscall.Mprotect(nil, 0)
}

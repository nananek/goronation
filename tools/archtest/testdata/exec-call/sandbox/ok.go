package sandbox

import "syscall"

func run() error {
	return syscall.Exec("/bin/true", nil, nil)
}

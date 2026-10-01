//go:build unix

package eventlog

import "syscall"

func umask(m int) int { return syscall.Umask(m) }

//go:build linux || darwin

package conformance

import "syscall"

// cttyAttr は、pty の slave (子の fd 3) を、制御端末にして起動する属性 (新しい session で、制御端末を持つ)。
func cttyAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
}

//go:build !(linux || darwin)

package conformance

import "syscall"

// cttyAttr は、この OS では、制御端末を持つ子を起こせない (nil を返す。端末の項目は、pty の用意で、先に落ちる)。
func cttyAttr() *syscall.SysProcAttr { return nil }

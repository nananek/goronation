//go:build linux

package conformance

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPTY は、pty の master と slave を開く (slave は、端末エミュレータが渡す、/dev/pts/N の形)。
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock, n int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("pty を unlock できない: %w", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("pty の番号を取れない: %w", e)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

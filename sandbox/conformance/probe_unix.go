//go:build linux || darwin

package conformance

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// preadFd は、fd の先頭から buf に読む (fd の位置は動かさない)。
func preadFd(fd int, buf []byte) (int, error) { return syscall.Pread(fd, buf, 0) }

// osNoCTTY は、端末を開くとき、制御端末にしない flag (O_NOCTTY)。
const osNoCTTY = syscall.O_NOCTTY

// killSelf は、自分に SIGKILL を送る。
func killSelf() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) }

// kill0 は、pid にシグナル 0 を送る (在るかの確認)。
func kill0(pid int) error { return syscall.Kill(pid, 0) }

// isESRCH は、err が、そのプロセスが無いことか。
func isESRCH(err error) bool { return errors.Is(err, syscall.ESRCH) }

// spawnDetached は、自分自身を、-sleep marker の probe として、setsid して起こす (親の process group から外れる)。
func spawnDetached(marker string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, probeArg, "-sleep", marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// tiocstiOn は、fd の端末に、TIOCSTI で 1 文字を注入しようとする。注入できたら ""、できなければ、理由。
func tiocstiOn(fd int) string {
	ch := byte('x')
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCSTI), uintptr(unsafe.Pointer(&ch))); e != 0 {
		return e.Error()
	}
	return ""
}

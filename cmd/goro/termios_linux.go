package main

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"unsafe"
)

// termState は、端末 (fd) の termios。檻は端末に直結するので、檻の中のプロセスは、端末の設定 (raw・-echo・-isig など) を
// 変えられる。goro run は、檻の起動の前に保存し、檻が終わった後に戻す (壊れた端末を、利用者の手元に残さない)。
type termState struct {
	fd int
	t  syscall.Termios
}

// ioctl は、fd に ioctl の req を、arg を指す構造体で発行する。
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// saveTermios は、fd の termios を保存する。fd が端末でなければ (ENOTTY)、nil と nil を返す (戻すものが無い)。
func saveTermios(fd int) (*termState, error) {
	s := &termState{fd: fd}
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&s.t)); err != nil {
		if errors.Is(err, syscall.ENOTTY) {
			return nil, nil
		}
		return nil, err
	}
	return s, nil
}

// restore は、保存した termios を、すぐに戻す。s が nil (端末でなかった) なら何もしない。
func (s *termState) restore() error {
	if s == nil {
		return nil
	}
	return ioctl(s.fd, syscall.TCSETS, unsafe.Pointer(&s.t))
}

// restoreTermios は、s を戻し、失敗したら警告を出す。端末が切れた (hangup。EIO) ときは、戻す先が無いので黙る。
func restoreTermios(s *termState, stderr io.Writer) {
	if err := s.restore(); err != nil && !errors.Is(err, syscall.EIO) {
		fmt.Fprintf(stderr, "goro run: 端末の設定を戻せない (stty sane で戻す): %v\n", err)
	}
}

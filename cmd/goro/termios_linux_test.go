package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// openPty は、pty の master と slave を開く (標準ライブラリだけ。/dev/ptmx を開いて、slave の鍵を外す)。
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("/dev/ptmx を開けない: %v", err)
	}
	var unlock int32
	var n uint32
	ctl(t, m, func(fd int) error { return ioctl(fd, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)) })
	ctl(t, m, func(fd int) error { return ioctl(fd, syscall.TIOCGPTN, unsafe.Pointer(&n)) })
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("pty の slave を開けない: %v", err)
	}
	t.Cleanup(func() { s.Close(); m.Close() })
	return m, s
}

// ctl は、f の fd で fn を実行する (f.Fd() を使うと、fd が blocking になり、master の Close が読みを起こせなくなる)。
func ctl(t *testing.T, f *os.File, fn func(fd int) error) {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		t.Fatal(err)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
}

// termOf は、f (端末) の termios を読む。
func termOf(t *testing.T, f *os.File) syscall.Termios {
	t.Helper()
	var tm syscall.Termios
	ctl(t, f, func(fd int) error { return ioctl(fd, syscall.TCGETS, unsafe.Pointer(&tm)) })
	return tm
}

// setTermOf は、f (端末) の termios を設定する。
func setTermOf(t *testing.T, f *os.File, tm syscall.Termios) {
	t.Helper()
	ctl(t, f, func(fd int) error { return ioctl(fd, syscall.TCSETS, unsafe.Pointer(&tm)) })
}

// rawOf は、tm を、raw・-echo・-isig にしたもの (stty raw -echo -isig 相当)。
func rawOf(tm syscall.Termios) syscall.Termios {
	tm.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	tm.Iflag &^= syscall.ICRNL | syscall.IXON | syscall.INLCR | syscall.ISTRIP
	tm.Oflag &^= syscall.OPOST
	tm.Cc[syscall.VMIN], tm.Cc[syscall.VTIME] = 1, 0
	return tm
}

// saveTermios・restore: 端末の設定を保存して、raw・-echo・-isig にした後、restore で、全部 (Cc を含む) 元に戻る。
// 端末でない fd は、nil (戻すものが無い) で、nil の restore は何もしない。
func TestTermiosSaveRestore(t *testing.T) {
	_, slave := openPty(t)
	before := termOf(t, slave)
	var st *termState
	ctl(t, slave, func(fd int) (err error) { st, err = saveTermios(fd); return })
	if st == nil {
		t.Fatal("端末なのに、saveTermios が nil")
	}
	setTermOf(t, slave, rawOf(before))
	if termOf(t, slave) == before {
		t.Fatal("前提: raw にしたのに、termios が変わっていない")
	}
	if err := st.restore(); err != nil {
		t.Fatal(err)
	}
	if after := termOf(t, slave); after != before {
		t.Errorf("restore の後の termios が、保存した時と違う:\n before %+v\n after  %+v", before, after)
	}
	// 保存した後に、別の値へ変えて restore しても、保存した値になる (restore が、現在の値を保存し直さない)。
	setTermOf(t, slave, rawOf(before))
	st.restore()
	if termOf(t, slave) != before {
		t.Error("2 回目の restore が、保存した値に戻さない")
	}

	// 端末でない fd (/dev/null・pipe): nil と nil。nil の restore は何もしない。
	null, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	ctl(t, null, func(fd int) error {
		if s, err := saveTermios(fd); s != nil || err != nil {
			t.Errorf("/dev/null: saveTermios = %v, %v, want nil, nil", s, err)
		}
		return nil
	})
	if err := (*termState)(nil).restore(); err != nil {
		t.Errorf("nil の restore = %v", err)
	}
	// 開いていない fd は、端末でない以外の error (警告を出す)。
	if s, err := saveTermios(-1); s != nil || err == nil {
		t.Errorf("saveTermios(-1) = %v, %v, want nil と error", s, err)
	}
	var w strings.Builder
	restoreTermios(&termState{fd: -1}, &w)
	if !strings.Contains(w.String(), "端末の設定を戻せない") {
		t.Errorf("戻せなかったときの警告が無い: %q", w.String())
	}
}

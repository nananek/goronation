//go:build linux

package contract

import (
	"slices"
	"syscall"
	"testing"
)

// fdFlags は、fd の close-on-exec の flag (F_GETFD)。
func fdFlags(t *testing.T, fd int) int {
	t.Helper()
	r, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if e != 0 {
		t.Fatalf("fcntl(%d, F_GETFD): %v", fd, e)
	}
	return int(r)
}

func isCloexec(t *testing.T, fd int) bool {
	t.Helper()
	return fdFlags(t, fd)&syscall.FD_CLOEXEC != 0
}

// openInherited は、CLOEXEC でない fd を開く (継承した fd の見立て。os.OpenFile は CLOEXEC を付けるので、syscall.Open を使う)。
func openInherited(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Open("/dev/null", syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if isCloexec(t, fd) {
		t.Fatalf("テストの前提: fd %d は CLOEXEC でない", fd)
	}
	return fd
}

// TestCloseOnExecFrom は、first 以降 (first を含む) の fd を close-on-exec にし、それより下の fd は変えないことを、
// close_range と /proc/self/fd の両方の経路で確認する (後者は、close_range の番号が無い arch として、差し込む)。
func TestCloseOnExecFrom(t *testing.T) {
	for name, useProc := range map[string]bool{"close_range (使えれば)": false, "/proc/self/fd": true} {
		t.Run(name, func(t *testing.T) {
			if useProc {
				old := closeRangeNR
				t.Cleanup(func() { closeRangeNR = old })
				closeRangeNR = func() (uintptr, bool) { return 0, false }
			}
			a, b := openInherited(t), openInherited(t)
			lo, hi := min(a, b), max(a, b)
			far := 700 // 大きな番号の fd も、範囲に入る
			if err := syscall.Dup3(lo, far, 0); err != nil {
				t.Skipf("fd %d を作れない: %v", far, err)
			}
			t.Cleanup(func() { syscall.Close(far) })

			if err := closeOnExecFrom(hi); err != nil {
				t.Fatal(err)
			}
			if isCloexec(t, lo) {
				t.Errorf("first (%d) より下の fd %d が、close-on-exec になった", hi, lo)
			}
			if !isCloexec(t, hi) {
				t.Errorf("first と同じ fd %d が、close-on-exec にならない (first を含むはず)", hi)
			}
			if !isCloexec(t, far) {
				t.Errorf("大きな番号の fd %d が、close-on-exec にならない", far)
			}

			var std []int
			for fd := 0; fd <= 2; fd++ {
				std = append(std, fdFlags(t, fd))
			}
			if err := closeOnExecFrom(3); err != nil {
				t.Fatal(err)
			}
			if !isCloexec(t, lo) && lo >= 3 {
				t.Errorf("first=3 で、fd %d が close-on-exec にならない", lo)
			}
			var after []int
			for fd := 0; fd <= 2; fd++ {
				after = append(after, fdFlags(t, fd))
			}
			if !slices.Equal(std, after) {
				t.Errorf("標準入出力 (0〜2) の flag が変わった: %v → %v", std, after)
			}
		})
	}
}

// TestCloseOnExecFromFallsBackToProc は、close_range が失敗したとき (5.9 未満の ENOSYS・5.9〜5.10 の EINVAL・seccomp の EPERM)、
// /proc/self/fd を辿ることを確認する。存在しない syscall 番号 (ENOSYS になる) を、close_range の番号として差し込む。
func TestCloseOnExecFromFallsBackToProc(t *testing.T) {
	old := closeRangeNR
	t.Cleanup(func() { closeRangeNR = old })
	closeRangeNR = func() (uintptr, bool) { return 9999, true }

	fd := openInherited(t)
	if err := closeOnExecFrom(fd); err != nil {
		t.Fatal(err)
	}
	if !isCloexec(t, fd) {
		t.Errorf("close_range が失敗したのに、fd %d が close-on-exec にならない (/proc に切り替わらない)", fd)
	}

	// 番号が無い arch (mips など) も、/proc を辿る。
	closeRangeNR = func() (uintptr, bool) { return 0, false }
	fd2 := openInherited(t)
	if err := closeOnExecFrom(fd2); err != nil {
		t.Fatal(err)
	}
	if !isCloexec(t, fd2) {
		t.Errorf("番号が無いのに、fd %d が close-on-exec にならない", fd2)
	}
}

// TestCloseOnExecFromFailsClosed は、close_range が使えず、/proc/self/fd も辿れないとき、成功にせず error にする
// (継承した fd が檻に届くまま、起動しない) ことを確認する。
func TestCloseOnExecFromFailsClosed(t *testing.T) {
	oldNR, oldDir := closeRangeNR, procSelfFd
	t.Cleanup(func() { closeRangeNR, procSelfFd = oldNR, oldDir })
	closeRangeNR = func() (uintptr, bool) { return 0, false }
	procSelfFd = t.TempDir() + "/none"
	if err := closeOnExecFrom(3); err == nil {
		t.Fatal("どちらの経路も使えないのに、error にならない")
	}
}

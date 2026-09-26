//go:build unix

package hostfs

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRejectsSpecialFiles は、FIFO・ソケット・デバイスを、開かずに拒否することを確認する。writer の無い FIFO を
// 開こうとして止まる実装が、テストを止めないよう、時間を区切る。
func TestRejectsSpecialFiles(t *testing.T) {
	root, dir := newRoot(t, nil)
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ソケットは、sun_path の上限 (107 バイト) に収まる短い path に作り、root の中へは、名前の変更で入れる。
	short, err := os.MkdirTemp("/tmp", "hf")
	if err != nil {
		t.Skipf("短い一時ディレクトリを作れない: %v", err)
	}
	defer os.RemoveAll(short)
	l, err := net.Listen("unix", filepath.Join(short, "s"))
	if err != nil {
		t.Skipf("ソケットを作れない: %v", err)
	}
	defer l.Close()
	if err := os.Rename(filepath.Join(short, "s"), filepath.Join(dir, "sock")); err != nil {
		t.Skipf("ソケットを root の中へ移せない: %v", err)
	}
	names := []string{"fifo", "sock"}
	if err := syscall.Mknod(filepath.Join(dir, "dev"), syscall.S_IFCHR|0o644, 1<<8|3); err == nil { // /dev/null と同じ番号
		names = append(names, "dev")
	} else {
		t.Logf("デバイスファイルを作れない (権限が無い): %v", err)
	}
	for _, name := range names {
		within(t, 5*time.Second, func() {
			f, err := Open(root, name, 100)
			if f != nil {
				f.Close()
			}
			if !errors.Is(err, ErrNotRegular) {
				t.Errorf("Open(%q) = %v, want ErrNotRegular", name, err)
			}
		})
	}
}

// TestFIFOSwappedBeforeOpen は、確認の後、open の前に、名前が FIFO に差し替えられても、止まらずに拒否することを確認する
// (O_NONBLOCK)。
func TestFIFOSwappedBeforeOpen(t *testing.T) {
	root, dir := newRoot(t, map[string]string{"f": "orig"})
	var f *os.File
	var err error
	within(t, 5*time.Second, func() {
		f, err = open(root, "f", 100, func(stage string) {
			if stage == stageBeforeOpen {
				p := filepath.Join(dir, "f")
				os.Remove(p)
				if e := syscall.Mkfifo(p, 0o644); e != nil {
					t.Error(e)
				}
			}
		})
	})
	if f != nil {
		f.Close()
	}
	if err == nil {
		t.Error("FIFO に差し替えられたのに、開けた")
	}
	if !errors.Is(err, ErrNotRegular) && !errors.Is(err, ErrChanged) {
		t.Errorf("error = %v, want ErrNotRegular か ErrChanged", err)
	}
}

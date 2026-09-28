package main

import (
	"bytes"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// inqOf は、端末 f の入力キューにある、読める (確定した行の) バイト数 (FIONREAD)。
func inqOf(t *testing.T, f interface {
	SyscallConn() (syscall.RawConn, error)
}) int32 {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var n int32
	var ierr error
	if err := rc.Control(func(fd uintptr) { ierr = ioctl(int(fd), syscall.TIOCINQ, unsafe.Pointer(&n)) }); err != nil {
		t.Fatal(err)
	}
	if ierr != nil {
		t.Fatal(ierr)
	}
	return n
}

// 攻撃レビュー: goronation auth は、値の 1 行を読み終えて ECHO を戻すとき、端末の入力キューに残った未読の入力 (貼り付けの 2 行目以降) を捨てない。
// 終了後に親のシェルがそれを読んで実行する。ECHO が切れている間の貼り付けは画面に出ないので、利用者は気づけない
// (クリップボードに 2 行目以降を仕込む pastejacking)。sudo・getpass(3) は、TCSAFLUSH で捨てる。
// 同じ原因で、入力の途中 (改行なし) に外から SIGTERM・SIGHUP で止められると、入力途中の値の断片が、親のシェルのプロンプトに現れる
// (tmux で確認。Ctrl-C は、端末のドライバが入力を捨てるので起きない)。
func TestAttackAuthDiscardsTypeAheadOnTerminal(t *testing.T) {
	master, slave := openPty(t)
	// 貼り付け: 値の 1 行目と、追加の 2 行目。読み取りの始まる前に、入力キューへ入れておく (順序を決める)。
	if _, err := master.WriteString(authTok + "\n" + "touch /tmp/injected-by-paste\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for inqOf(t, slave) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("貼り付けが、端末の入力キューに入らない")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var out, errb bytes.Buffer
	if code := runAuth([]string{"github", "--state-dir", t.TempDir()}, slave, &out, &errb); code != 0 {
		t.Fatalf("終了コード %d\nstderr: %s", code, errb.String())
	}
	if n := inqOf(t, slave); n != 0 {
		t.Errorf("終了後も、端末の入力キューに、未読の %d バイトが残る (親のシェルが、貼り付けの 2 行目以降を読んで実行する)", n)
	}
}

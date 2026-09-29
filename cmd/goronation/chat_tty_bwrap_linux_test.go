package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 制御端末 (/dev/tty) の確認 (B3 のゲートの続き)。chat セッションは、標準入出力を socketpair にして、pty を使わない。だが、goronation serve --chat を、
// 端末から動かすと、serve は制御端末を持ち、檻が --new-session なしで起動するなら、檻の中のプロセスは、bwrap の /dev/tty から、運用者の端末
// に、制御列を書ける (タイトル・画面の消去・色)。運用者のキー入力も、読める。chat の檻は、制御端末を持たない (--new-session) ようにする。

const ttyMarker = "PWNED-TTY"

// fakeChatTTYProbe は、偽のエージェント: /dev/tty を開いて、書く・読む。結果は、標準エラー出力 (serve の標準エラー出力) に出す。
func fakeChatTTYProbe() int {
	w, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tty-open => "+err.Error())
		return 0
	}
	w.WriteString("\x1b]0;" + ttyMarker + "\x07\x1b[31m" + ttyMarker + "\x1b[0m\n")
	w.Close()
	fmt.Fprintln(os.Stderr, "tty-open => OPENED-AND-WROTE")
	return 0
}

// TestChatSessionTTYChild は、TestChatSessionDoesNotShareControllingTerminal が、制御端末つきで起こす子。単独では何もしない。
func TestChatSessionTTYChild(t *testing.T) {
	if os.Getenv("GORONATION_TTY_CHILD") == "" {
		t.Skip("TestChatSessionDoesNotShareControllingTerminal の子")
	}
	s, _, stderr := startChatFixture(t, "tty-probe")
	s.Wait()
	fmt.Print(stderr.String())
}

// 赤: 制御端末を持つ serve から起こした chat の檻の中のプロセスが、その端末に書けてはいけない。
func TestChatSessionDoesNotShareControllingTerminal(t *testing.T) {
	requireBwrap(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)")
	}
	master, slave, err := openHostPty()
	if err != nil {
		t.Skip("pty を開けない: " + err.Error())
	}
	defer master.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestChatSessionTTYChild$", "-test.v")
	cmd.Env = append(os.Environ(), "GORONATION_TTY_CHILD=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0} // slave (子の fd 0) を、子の制御端末にする
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	var got bytes.Buffer
	readDone := make(chan struct{})
	go func() { io.Copy(&got, master); close(readDone) }()
	cmd.Wait()
	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
	}
	out := got.String()
	if !strings.Contains(out, "tty-open => ") {
		t.Fatalf("子の、/dev/tty を開く試みが動いていない:\n%s", out)
	}
	if strings.Contains(out, "tty-open => OPENED-AND-WROTE") || strings.Contains(out, "\x1b]0;"+ttyMarker) {
		t.Errorf("chat の檻の中のプロセスが、serve の制御端末を開いて、制御列を書けた (--new-session が無い):\n%q", out)
	}
}

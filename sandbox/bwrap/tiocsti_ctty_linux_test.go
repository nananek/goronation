package bwrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// このファイルは、「標準入出力が端末でなくても、制御端末を継承していれば、檻はホストの端末に届く」ことの確認。
// TIOCSTI は、呼んだプロセスの制御端末 (/dev/tty) に効く。bwrap は --new-session が無いと setsid しないので、
// 檻は、Start を呼んだプロセスの制御端末を、そのまま持つ (--dev /dev の /dev/tty は、ホストの /dev/tty)。
// Start の TIOCSTI の確認が、標準入出力だけを見ると、`goro run > log 2>&1 < /dev/null` の形で素通りする。
//
// テストバイナリ自身が、init() で、環境変数 tiocstiRoleEnv に応じて、ホスト側 (制御端末を持つ Start の呼び手) と、
// 檻の中 (/dev/tty を開いて TIOCSTI を試す) の役を演じる (TestMain は変えない)。
const (
	tiocstiRoleEnv    = "GORO_BWRAP_TIOCSTI_ROLE" // "host": 制御端末を持つ Start の呼び手 / "cage": 檻の中
	tiocstiSessionEnv = "GORO_BWRAP_TIOCSTI_NEWSESSION"
	cageWroteMark     = "CAGE-WROTE-TO-HOST-TTY"
)

func init() {
	switch os.Getenv(tiocstiRoleEnv) {
	case "host":
		tiocstiHostMain()
		os.Exit(0)
	case "cage":
		tiocstiCageMain()
		os.Exit(0)
	}
}

// tiocstiHostMain は、制御端末を持つが、標準入出力は端末でないプロセスとして、TIOCSTI が有効な (と、sysctl が言う)
// ホストで、Start を呼ぶ。結果は標準出力に "START_ERR:" か "STARTED" で書く。
func tiocstiHostMain() {
	_ = syscall.Close(3) // 制御端末にした pty の slave (fd 3) は閉じる (制御端末は、閉じても残る)
	dir, _ := os.MkdirTemp("", "tiocsti")
	tiocstiPath = filepath.Join(dir, "legacy_tiocsti")
	_ = os.WriteFile(tiocstiPath, []byte("1\n"), 0o644) // TIOCSTI が有効なホスト (kernel 6.1 以前は、常に有効)
	exe, _ := os.Executable()
	s := Spec{
		Host:       Host{Home: "/nonexistent-home"},
		Symlinks:   usrSymlinks(),
		Binds:      []Bind{{Src: "/usr", Dst: "/usr"}, {Src: exe, Dst: "/opt/probe/probe"}},
		Env:        []EnvVar{{tiocstiRoleEnv, "cage"}},
		Cmd:        []string{"/opt/probe/probe"},
		NewSession: os.Getenv(tiocstiSessionEnv) == "1",
		Stdout:     os.Stdout, // pipe (端末ではない)
		Stderr:     os.Stderr, // pipe (端末ではない)
		// Stdin は nil (/dev/null)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, s)
	if err != nil {
		fmt.Println("START_ERR:", err)
		return
	}
	fmt.Println("STARTED")
	fmt.Println("CAGE_EXIT:", c.Wait())
}

// tiocstiCageMain は、檻の中で、制御端末 (/dev/tty) を開き、書き込み・TIOCSTI を試して、結果を標準出力に書く。
func tiocstiCageMain() {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		fmt.Println("CAGE_OPEN_TTY:", err)
		return
	}
	defer f.Close()
	fmt.Println("CAGE_OPEN_TTY: ok")
	_, _ = f.WriteString(cageWroteMark + "\n")
	ch := byte('x')
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSTI, uintptr(unsafe.Pointer(&ch)))
	fmt.Printf("CAGE_TIOCSTI: errno=%d (%v)\n", int(e), e)
}

// runWithControllingTTY は、pty を制御端末にし、標準入出力は端末でないプロセスとして、ホスト側の役を動かす。
// 出力 (標準出力・標準エラー) と、pty の master に出てきたもの (ホストの端末に表示されるもの) を返す。
func runWithControllingTTY(t *testing.T, newSession bool) (out, ttyOut string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	master, slave := openPty(t)
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), tiocstiRoleEnv+"=host", fmt.Sprintf("%s=%v", tiocstiSessionEnv, map[bool]string{true: "1", false: "0"}[newSession]))
	cmd.ExtraFiles = []*os.File{slave} // fd 3
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	got := make(chan string, 1)
	go func() { // pty の master に出てくるもの
		var b bytes.Buffer
		tmp := make([]byte, 4096)
		for {
			n, err := master.Read(tmp)
			b.Write(tmp[:n])
			if err != nil {
				break
			}
		}
		got <- b.String()
	}()
	if err := cmd.Run(); err != nil {
		t.Fatalf("ホスト側の役が失敗した: %v\n%s", err, buf.String())
	}
	slave.Close()
	time.Sleep(200 * time.Millisecond)
	master.Close()
	select {
	case s := <-got:
		ttyOut = s
	case <-time.After(3 * time.Second):
	}
	return buf.String(), ttyOut
}

// TestStartChecksTIOCSTIWithControllingTerminal は、標準入出力が端末でなくても、制御端末を持ったまま
// NewSession なしで起動するとき、TIOCSTI が有効なら、起動しないことを確認する。
func TestStartChecksTIOCSTIWithControllingTerminal(t *testing.T) {
	needBwrap(t)
	out, ttyOut := runWithControllingTTY(t, false)
	t.Logf("host role output:\n%s", out)
	t.Logf("host terminal (pty master) received: %q", ttyOut)
	if strings.Contains(out, "START_ERR:") && strings.Contains(out, "TIOCSTI") {
		return
	}
	t.Errorf("制御端末を持ち、TIOCSTI が有効なのに、Start が断らなかった (檻が /dev/tty を開ける: %v, 檻が書いた文字がホストの端末に出た: %v)",
		strings.Contains(out, "CAGE_OPEN_TTY: ok"), strings.Contains(ttyOut, cageWroteMark))
}

// TestNewSessionCageHasNoControllingTerminal は、NewSession なら、檻が制御端末を持たない (/dev/tty を開けない) ことの対照。
func TestNewSessionCageHasNoControllingTerminal(t *testing.T) {
	needBwrap(t)
	out, ttyOut := runWithControllingTTY(t, true)
	t.Logf("host role output:\n%s", out)
	if !strings.Contains(out, "STARTED") || !strings.Contains(out, "CAGE_OPEN_TTY:") || strings.Contains(out, "CAGE_OPEN_TTY: ok") {
		t.Errorf("NewSession の檻が /dev/tty を開けた・起動しなかった:\n%s", out)
	}
	if strings.Contains(ttyOut, cageWroteMark) {
		t.Errorf("NewSession の檻が、ホストの端末に書けた")
	}
}

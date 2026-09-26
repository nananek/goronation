package bwrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// このファイルは、Start を呼ぶプロセスが持つ、CLOEXEC でない fd が、檻に届かないことの確認。
// Go の os/exec は、Stdin・Stdout・Stderr と ExtraFiles 以外の fd を、閉じない (自分が開いた fd だけ CLOEXEC にする)。
// 親プロセスから継承した fd (systemd の socket activation・シェルの `exec 9<file`・起動した側が渡した pipe など) は、
// bwrap が閉じないので (実 bwrap 0.12.0 で確認)、そのまま檻のコマンドに届く (/proc/self/fd に見え、読める)。
// doc.go の「Spec に無いものは、檻に入らない」と食い違う。
//
// テストバイナリ自身が、init() で、環境変数 fdleakRoleEnv が "host" のとき、Start の呼び手の役を演じる (TestMain は変えない)。
const (
	fdleakRoleEnv = "GORO_BWRAP_FDLEAK_ROLE"
	fdleakMarker  = "FDLEAK-MARKER-CONTENT"
	fdleakFd      = 9
)

func init() {
	if os.Getenv(fdleakRoleEnv) == "host" {
		fdleakHostMain()
		os.Exit(0)
	}
}

// fdleakHostMain は、fd 9 (CLOEXEC でない) を持ったまま Start を呼び、檻の中で fd 9 を読む。
func fdleakHostMain() {
	s := Spec{
		Host:     Host{Home: "/nonexistent-home"},
		Symlinks: usrSymlinks(),
		Binds:    []Bind{{Src: "/usr", Dst: "/usr"}},
		Env:      []EnvVar{{"PATH", "/usr/bin:/bin"}},
		Cmd:      []string{"/usr/bin/sh", "-c", fmt.Sprintf("ls /proc/self/fd; cat <&%d 2>&1; echo cage-done", fdleakFd)},
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, s)
	if err != nil {
		fmt.Println("START_ERR:", err)
		return
	}
	fmt.Println("CAGE_EXIT:", c.Wait())
}

func TestStartDoesNotLeakInheritedFds(t *testing.T) {
	needBwrap(t)
	f, err := os.CreateTemp(t.TempDir(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(fdleakMarker + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), fdleakRoleEnv+"=host")
	// fd 3..8 は閉じ、fd 9 に f を、CLOEXEC なしで持たせる (起動した側が渡した fd の見立て)。
	cmd.ExtraFiles = make([]*os.File, fdleakFd-3+1)
	cmd.ExtraFiles[fdleakFd-3] = f
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("ホスト側の役が失敗した: %v\n%s", err, out.String())
	}
	t.Logf("cage output:\n%s", out.String())
	if strings.Contains(out.String(), fdleakMarker) {
		t.Errorf("Start を呼ぶプロセスの fd %d が、檻の中で読めた (%s)", fdleakFd, fdleakMarker)
	}
	if !strings.Contains(out.String(), "cage-done") {
		t.Errorf("檻が動かなかった:\n%s", out.String())
	}
}

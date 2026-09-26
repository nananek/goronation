package bwrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// このファイルは、継承した fd の close-on-exec 化が、fd 3 (標準入出力の次) から始まることを、Start の全体で確かめる
// (fdleak_linux_test.go は fd 9 だけ)。fd 3 は、Go のプロセスでは、ランタイムが使いうる番号なので、
// 子プロセスの ExtraFiles の先頭 (= fd 3) に置く。子プロセスは、init() で fdleakNRoleEnv の役 (値は fd 番号) を演じる。
const fdleakNRoleEnv = "GORO_BWRAP_FDLEAKN_ROLE"

func init() {
	if fd := os.Getenv(fdleakNRoleEnv); fd != "" {
		fdleakNHostMain(fd)
		os.Exit(0)
	}
}

// fdleakNHostMain は、継承した fd fd を持ったまま Start を呼び、檻の中で、それを読む。
func fdleakNHostMain(fd string) {
	s := Spec{
		Host:     Host{Home: "/nonexistent-home"},
		Symlinks: usrSymlinks(),
		Binds:    []Bind{{Src: "/usr", Dst: "/usr"}},
		Env:      []EnvVar{{"PATH", "/usr/bin:/bin"}},
		Cmd:      []string{"/usr/bin/sh", "-c", fmt.Sprintf("cat <&%s 2>&1; echo cage-done", fd)},
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

// TestStartDoesNotLeakLowestInheritedFds は、fd 3 (境界) と 4 が、檻に届かないことを確認する。
func TestStartDoesNotLeakLowestInheritedFds(t *testing.T) {
	needBwrap(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range []int{3, 4} {
		t.Run("fd "+strconv.Itoa(fd), func(t *testing.T) {
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
			cmd := exec.Command(exe)
			cmd.Env = append(os.Environ(), fdleakNRoleEnv+"="+strconv.Itoa(fd))
			cmd.ExtraFiles = make([]*os.File, fd-3+1) // ExtraFiles[i] は fd 3+i
			cmd.ExtraFiles[fd-3] = f
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Run(); err != nil {
				t.Fatalf("ホスト側の役が失敗した: %v\n%s", err, out.String())
			}
			if strings.Contains(out.String(), fdleakMarker) {
				t.Errorf("Start を呼ぶプロセスの fd %d が、檻の中で読めた", fd)
			}
			if !strings.Contains(out.String(), "cage-done") {
				t.Errorf("檻が動かなかった:\n%s", out.String())
			}
		})
	}
}

// TestStartFailsWhenFdsCannotBeCloseOnExec は、継承した fd を close-on-exec にできないとき、檻を起動しないことを確認する
// (fail-closed)。bwrap は要らない (起動の前に断る)。
func TestStartFailsWhenFdsCannotBeCloseOnExec(t *testing.T) {
	old := closeInheritedFDs
	t.Cleanup(func() { closeInheritedFDs = old })
	closeInheritedFDs = func() error { return fmt.Errorf("テスト用の失敗") }
	s := Spec{Host: Host{Home: "/nonexistent-home"}, Binds: []Bind{{Src: "/usr", Dst: "/usr"}}, Cmd: []string{"/usr/bin/true"}}
	c, err := Start(context.Background(), s)
	if err == nil {
		c.Wait()
		t.Fatal("close-on-exec にできないのに、Start が起動した")
	}
	if c != nil || !strings.Contains(err.Error(), "close-on-exec") {
		t.Errorf("c = %v, err = %v", c, err)
	}
}

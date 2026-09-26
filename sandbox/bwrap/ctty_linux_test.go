package bwrap

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// このファイルは、制御端末の判定 (controllingTerminal・terminalAt) を、実物で確かめる。
// 制御端末を持つ・持たないは、プロセスごとの性質なので、テストバイナリ自身を子プロセスとして起動する
// (init() が cttyRoleEnv の役を演じる。TestMain の前に動くので、controllingTerminal は実物のまま)。
const cttyRoleEnv = "GORO_BWRAP_CTTY_ROLE" // "probe": controllingTerminal() の結果を出す

func init() {
	if os.Getenv(cttyRoleEnv) == "probe" {
		fmt.Printf("CTTY=%v\n", controllingTerminal())
		os.Exit(0)
	}
}

// runChild は、テストバイナリ自身を、env の役で起動して、出力を返す。標準入出力は pipe (端末ではない)。
// ctty が真なら、pty の slave を制御端末にする。偽なら、Setsid で切り離す (制御端末を持たない)。
// fd 3 には、どちらでも何かを置く (tiocsti_ctty_linux_test.go の host の役が、fd 3 を閉じるため)。
func runChild(t *testing.T, env []string, ctty bool) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), env...)
	if ctty {
		_, slave := openPty(t)
		cmd.ExtraFiles = []*os.File{slave} // fd 3
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
	} else {
		null, err := os.Open("/dev/null")
		if err != nil {
			t.Fatal(err)
		}
		defer null.Close()
		cmd.ExtraFiles = []*os.File{null} // fd 3
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("子プロセスが失敗した: %v\n%s", err, buf.String())
	}
	return buf.String()
}

// TestControllingTerminal は、制御端末を持つ子プロセスでは真、Setsid で切り離した子プロセスでは偽になることを確認する。
// 標準入出力は、どちらも pipe。
func TestControllingTerminal(t *testing.T) {
	if out := runChild(t, []string{cttyRoleEnv + "=probe"}, true); !strings.Contains(out, "CTTY=true") {
		t.Errorf("制御端末を持つ子プロセスで、controllingTerminal() が真にならない: %q", out)
	}
	if out := runChild(t, []string{cttyRoleEnv + "=probe"}, false); !strings.Contains(out, "CTTY=false") {
		t.Errorf("制御端末を持たない子プロセスで、controllingTerminal() が偽にならない: %q", out)
	}
}

// TestStartWithoutTerminalDoesNotCheckTIOCSTI は、標準入出力が端末でなく、制御端末も持たない (Setsid で切り離した)
// 呼び手は、TIOCSTI が有効 (と sysctl が言う) でも、NewSession なしで起動できることを確認する (確認するのは、端末に直結するときだけ)。
func TestStartWithoutTerminalDoesNotCheckTIOCSTI(t *testing.T) {
	needBwrap(t)
	out := runChild(t, []string{tiocstiRoleEnv + "=host", tiocstiSessionEnv + "=0"}, false)
	t.Logf("host role output:\n%s", out)
	if !strings.Contains(out, "STARTED") || strings.Contains(out, "START_ERR") {
		t.Errorf("端末に直結しないのに、Start が断った:\n%s", out)
	}
	if !strings.Contains(out, "CAGE_OPEN_TTY:") || strings.Contains(out, "CAGE_OPEN_TTY: ok") {
		t.Errorf("制御端末を持たない呼び手の檻が、/dev/tty を開けた・動かなかった:\n%s", out)
	}
}

// TestTerminalAt は、/dev/tty を開けないときの扱いを固定する。ENOENT (/dev/tty が無い) は持たない、
// それ以外 (確かめられない) は持つとみなす (fail-closed)。ENXIO (制御端末が無い) は、TestControllingTerminal が実物で確かめる。
func TestTerminalAt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	noperm := filepath.Join(dir, "noperm")
	if err := os.WriteFile(noperm, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		want       bool
		skip       bool
	}{
		{"開ける", file, true, false},
		{"ENOENT (無い)", filepath.Join(dir, "none"), false, false},
		{"ENOTDIR (確かめられない)", file + "/x", true, false},
		{"EISDIR (確かめられない)", dir, true, false},
		{"EACCES (確かめられない。root は開けるので skip)", noperm, true, os.Geteuid() == 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip {
				t.Skip("root は権限に依らず開ける")
			}
			if got := terminalAt(tc.path); got != tc.want {
				t.Errorf("terminalAt(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

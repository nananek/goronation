package bwrap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
)

// TestBackendProbe は、bwrap を使える環境で、Probe が nil を返すことを確認する (使えない環境は、skip。GORO_REQUIRE_BWRAP=1 では fail)。
func TestBackendProbe(t *testing.T) {
	needBwrap(t)
	if err := New(adapterHost).Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

// TestExitError は、Wait の結果を、契約の *ExitError にすることを確認する: 終了コード・シグナルでの死 (128 + 番号)・nil・別の error。
func TestExitError(t *testing.T) {
	if err := exitError(nil); err != nil {
		t.Errorf("exitError(nil) = %v", err)
	}
	plain := errors.New("wait: something else")
	if err := exitError(plain); err != plain {
		t.Errorf("別の error は、そのまま返す: %v", err)
	}
	for _, tc := range []struct {
		script string
		want   int
	}{{"exit 7", 7}, {"exit 1", 1}, {"kill -9 $$", 137}, {"kill -15 $$", 143}} {
		err := exitError(exec.Command("/bin/sh", "-c", tc.script).Run())
		var ee *sandbox.ExitError
		if !errors.As(err, &ee) || ee.Code != tc.want {
			t.Errorf("%q: %v, want *ExitError{%d}", tc.script, err, tc.want)
		}
	}
	if err := exitError(exec.Command("/bin/sh", "-c", "exit 0").Run()); err != nil {
		t.Errorf("正常終了: %v", err)
	}
}

// TestBackendStartRejectsBeforeLaunch は、契約に反する Spec を、起動せずに、ErrRejected で断ることを確認する (bwrap は要らない)。
func TestBackendStartRejectsBeforeLaunch(t *testing.T) {
	b := New(adapterHost)
	for name, s := range map[string]sandbox.Spec{
		"HOME 自体":  {Exec: "/opt/x/x", Read: []sandbox.Mount{{HostPath: "/home/u", GuestPath: "/x"}}},
		"資格情報の環境":  {Exec: "/opt/x/x", Env: []sandbox.EnvVar{{Key: "GH_TOKEN", Value: "x"}}},
		"相対の Exec": {Exec: "x"},
		// 契約の検証を通っても、bwrap 固有の検証で断るもの (種類は、同じ ErrRejected)。
		"GuestPath が /proc の中": {Exec: "/opt/x/x", Read: []sandbox.Mount{{HostPath: "/usr/bin", GuestPath: "/proc/x"}}}, // HostPath は実在する (Resolve は通る)
		// bwrap の検証には無い、契約の規則 (契約の共通検証を通らなければ、断る)。
		"Egress の親が Read に無い":  {Exec: "/opt/x/x", Egress: "/run/goro/proxy.sock"},
		"loopback でない待ち受け":     {Exec: "/opt/x/x", Loopback: []string{"0.0.0.0:3128"}},
		"Egress の親が Write のとき": {Exec: "/opt/x/x", Write: []sandbox.Mount{{HostPath: "/data/run", GuestPath: "/run/goro"}}, Egress: "/run/goro/proxy.sock"},
	} {
		c, err := b.Start(context.Background(), s)
		if c != nil || !errors.Is(err, sandbox.ErrRejected) {
			t.Errorf("%s: Start = %v, %v, want nil・ErrRejected", name, c, err)
		}
	}
}

// TestBackendStartTerminalUnsafe は、端末に直結して (Terminal = true)、TIOCSTI が有効なとき、起動せず、ErrTerminalUnsafe を返すことを確認する
// (bwrap は要らない: 検査は、起動の前)。Terminal = false (端末を持たない) なら、この検査は無い (起動は、bwrap を使えなければ失敗するが、ErrTerminalUnsafe ではない)。
func TestBackendStartTerminalUnsafe(t *testing.T) {
	setTIOCSTI(t, "1\n")
	old := controllingTerminal
	t.Cleanup(func() { controllingTerminal = old })
	controllingTerminal = func() bool { return true }
	b := New(adapterHost)
	s := sandbox.Spec{Exec: "/usr/bin/true", System: true, Terminal: true}
	if c, err := b.Start(context.Background(), s); c != nil || !errors.Is(err, sandbox.ErrTerminalUnsafe) {
		t.Errorf("Terminal = true・TIOCSTI 有効: Start = %v, %v, want nil・ErrTerminalUnsafe", c, err)
	}
	setTIOCSTI(t, "") // 確かめられない (ファイルが無い) ときも、起動しない
	if c, err := b.Start(context.Background(), s); c != nil || !errors.Is(err, sandbox.ErrTerminalUnsafe) {
		t.Errorf("Terminal = true・TIOCSTI を確かめられない: Start = %v, %v, want nil・ErrTerminalUnsafe", c, err)
	}
	s.Terminal = false
	c, err := b.Start(context.Background(), s)
	if c != nil {
		_ = c.Wait() // bwrap を使える環境では、起動できる
	}
	if errors.Is(err, sandbox.ErrTerminalUnsafe) {
		t.Errorf("Terminal = false で、ErrTerminalUnsafe: %v", err)
	}
}

// TestBackendStartRunsContractResolve は、Start が、契約の Resolve (symlink を辿った実体の検証) を、bwrap の検証とは別に通すことを確認する。
// 契約の検証だけが知る機密 (bwrap 側の Host に無い path) を指す symlink は、bwrap の検証は通るが、契約の Resolve が断る。
func TestBackendStartRunsContractResolve(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	b := New(adapterHost)
	b.rules.Host.Secrets = []string{secret} // 契約の検証だけが、この path を機密として知っている
	s := sandbox.Spec{Exec: "/usr/bin/true", System: true, Read: []sandbox.Mount{{HostPath: link, GuestPath: "/x"}}}
	if _, err := b.Prepare(s); err != nil {
		t.Fatalf("Prepare (字面だけ) が、symlink の HostPath を断った: %v", err)
	}
	c, err := b.Start(context.Background(), s)
	if c != nil {
		_ = c.Wait() // 契約の Resolve が働かないと、起動してしまう
		t.Error("契約の Resolve が、機密を指す symlink を断らなかった")
	}
	if !errors.Is(err, sandbox.ErrRejected) {
		t.Errorf("Start = %v, want ErrRejected", err)
	}
}

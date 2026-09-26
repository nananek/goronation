//go:build linux

package bwrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/nananek/goronation/core/sandbox"
)

// probeTimeout は、Probe が、檻の中の /usr/bin/true を待つ上限。
const probeTimeout = 10 * time.Second

// probe は、bwrap が固定パスに在り、檻の中で /usr/bin/true を実行できるかを確かめる。無ければ ErrNotInstalled、使えなければ ErrUnusable を包んだ error。
// 起動は、Start (検証つき) を通す (Probe だけ、別の引数を組まない)。
func (b *Backend) probe(ctx context.Context) error {
	if _, err := os.Stat(bwrapPath); err != nil {
		return fmt.Errorf("%w: %s: %v", sandbox.ErrNotInstalled, bwrapPath, err)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var out bytes.Buffer
	s := Spec{
		Host: Host{Home: "/nonexistent-home"},
		Symlinks: []Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		NewSession: true,
		Binds:      []Bind{{Src: "/usr", Dst: "/usr"}},
		Cmd:        []string{"/usr/bin/true"},
		Stdout:     &out,
		Stderr:     &out,
	}
	c, err := Start(ctx, s)
	if err != nil {
		return fmt.Errorf("%w: %v", sandbox.ErrUnusable, err)
	}
	if err := c.Wait(); err != nil {
		return fmt.Errorf("%w: 檻の中で /usr/bin/true を実行できない: %v: %s", sandbox.ErrUnusable, err, bytes.TrimSpace(out.Bytes()))
	}
	return nil
}

// start は、s を、契約の検証 (Validate・Resolve)・この package の検証 (Argv・symlink を辿った実体) の全部に通し、bwrap で起動する。
// どれかに通らなければ起動せず、ErrRejected を、端末への注入を塞げると確かめられなければ ErrTerminalUnsafe を包んだ error を返す。
func (b *Backend) start(ctx context.Context, s sandbox.Spec) (sandbox.Cage, error) {
	if err := b.rules.Validate(s); err != nil {
		return nil, err
	}
	bs := b.translate(s)
	if _, err := bs.Argv(); err != nil {
		return nil, fmt.Errorf("%w: %v", sandbox.ErrRejected, err)
	}
	if _, err := b.rules.Resolve(s); err != nil {
		return nil, err
	}
	// Start が行う検査を、先に同じ条件で行い、error の種類を分ける (Start の中の検査は、変えない)。
	if !bs.NewSession && (stdioIsTerminal(bs.Stdin, bs.Stdout, bs.Stderr) || controllingTerminal()) {
		if err := checkTIOCSTI(); err != nil {
			return nil, fmt.Errorf("%w: %v", sandbox.ErrTerminalUnsafe, err)
		}
	}
	if _, err := bs.withResolvedSrcs(); err != nil {
		return nil, fmt.Errorf("%w: %v", sandbox.ErrRejected, err)
	}
	c, err := Start(ctx, bs)
	if err != nil {
		return nil, err
	}
	return &cage{cmd: c}, nil
}

// cage は、Start が起動した檻 (*Cmd) を、契約の Cage にする。
type cage struct {
	cmd *Cmd
}

// Wait は、檻のコマンドの終了を待つ。異常終了は *sandbox.ExitError (シグナルで死んだら、128 + 番号)。
func (c *cage) Wait() error {
	return exitError(c.cmd.Wait())
}

// Signal は、bwrap にシグナルを送る (bwrap が、檻のコマンドを止める)。
func (c *cage) Signal(sig os.Signal) error {
	return c.cmd.Signal(sig)
}

// exitError は、Wait の結果を、契約の error にする。終了コードで終わったら *sandbox.ExitError、それ以外の error はそのまま。
func exitError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return &sandbox.ExitError{Code: 128 + int(ws.Signal())}
	}
	return &sandbox.ExitError{Code: ee.ExitCode()}
}

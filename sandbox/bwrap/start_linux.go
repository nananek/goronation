//go:build linux

package bwrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// tiocstiPath は、TIOCSTI (端末の入力に、キーを注入する ioctl) を有効にするかを決める sysctl (Linux 6.2 以降)。
var tiocstiPath = "/proc/sys/dev/tty/legacy_tiocsti"

// waitDelay は、コンテキストを取り消して bwrap を kill したあと、標準入出力の中継を待つ猶予。
const waitDelay = 2 * time.Second

// Cmd は、Start が起動した檻。
type Cmd struct {
	cmd *exec.Cmd
}

// Start は、s を検証し (symlink を辿った実際の path も、同じ規則で検証する)、檻を起動する。
// bwrap には、環境変数を渡さない (檻の環境変数は、s.Env だけ)。NewSession が false で、標準入出力のどれかが
// 端末なら、TIOCSTI が無効なことを確かめ、そうでなければ起動しない。
func Start(ctx context.Context, s Spec) (*Cmd, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if !s.NewSession && stdioIsTerminal(s.Stdin, s.Stdout, s.Stderr) {
		if err := checkTIOCSTI(); err != nil {
			return nil, err
		}
	}
	r, err := s.withResolvedSrcs()
	if err != nil {
		return nil, err
	}
	argv := r.argv()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("bwrap: 起動できない: %w", err)
	}
	return &Cmd{cmd: cmd}, nil
}

// Wait は、檻のコマンドの終了を待つ。異常終了は *exec.ExitError で返す (bwrap 自身の失敗も、終了コードで返る)。
func (c *Cmd) Wait() error {
	return c.cmd.Wait()
}

// Signal は、bwrap にシグナルを送る (bwrap が、檻のコマンドへ伝える)。
func (c *Cmd) Signal(sig os.Signal) error {
	return c.cmd.Process.Signal(sig)
}

// Pid は、ホストから見た bwrap のプロセス ID。
func (c *Cmd) Pid() int {
	return c.cmd.Process.Pid
}

// withResolvedSrcs は、各 bind の Src を、symlink を辿った実際の path に置き換えた Spec を返す。実際の path も、
// 同じ規則で検証する (機密の path を指す symlink を、機密でない名前で bind させないため)。bwrap には、実際の path を渡す。
func (s Spec) withResolvedSrcs() (Spec, error) {
	host := s.Host
	host.Home = evalOrSelf(host.Home)
	host.Secrets = slices.Clone(host.Secrets)
	for _, sec := range s.Host.Secrets {
		host.Secrets = append(host.Secrets, evalOrSelf(sec))
	}
	// 拒否する path (protectedTrees と、HOME の機密) 自体が symlink のとき、その先も拒否する。
	deny := slices.Clone(protectedTrees)
	for _, rel := range slices.Concat(homeSecrets, []string{".claude", ".claude.json", ".gnupg", ".gnupg-vault"}) {
		deny = append(deny, s.Host.Home+"/"+rel, host.Home+"/"+rel)
	}
	for _, p := range deny {
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			host.resolvedTrees = append(host.resolvedTrees, real)
		}
	}
	out := s
	out.Binds = slices.Clone(s.Binds)
	for i, b := range s.Binds {
		real, err := filepath.EvalSymlinks(b.Src)
		if err != nil {
			return Spec{}, fmt.Errorf("bwrap: Binds[%d]: Src %q を解決できない: %w", i, b.Src, err)
		}
		if err := host.checkSrc(b, real, false); err != nil {
			return Spec{}, fmt.Errorf("bwrap: Binds[%d]: Src %q は %q に解決され、拒否: %w", i, b.Src, real, err)
		}
		out.Binds[i].Src = real
	}
	return out, nil
}

// evalOrSelf は、p の symlink を辿った path (辿れなければ p)。
func evalOrSelf(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// checkTIOCSTI は、TIOCSTI が無効 (legacy_tiocsti が 0) なことを確かめる。確かめられなければ error にする。
// 端末に直結した檻は、制御端末を共有するので、TIOCSTI が有効だと、檻がホストの端末に、キーを注入できる。
func checkTIOCSTI() error {
	b, err := os.ReadFile(tiocstiPath)
	if err != nil {
		return fmt.Errorf("bwrap: 端末に直結して起動するが、TIOCSTI が無効なことを確かめられない (%w)。"+
			"kernel を 6.2 以降にして %s を 0 にするか、NewSession を使う", err, tiocstiPath)
	}
	if v := strings.TrimSpace(string(b)); v != "0" {
		return fmt.Errorf("bwrap: 端末に直結して起動するが、TIOCSTI が有効 (%s = %q)。"+
			"sysctl dev.tty.legacy_tiocsti=0 にするか、NewSession を使う", tiocstiPath, v)
	}
	return nil
}

// stdioIsTerminal は、標準入出力のどれかが、端末の *os.File か。*os.File 以外は、bwrap への pipe 越しになるので、端末ではない。
func stdioIsTerminal(std ...any) bool {
	for _, v := range std {
		if f, ok := v.(*os.File); ok && isTerminal(f) {
			return true
		}
	}
	return false
}

// isTerminal は、f が端末 (tty・pty・console) の文字デバイスか。デバイス番号を取れないときは、端末とみなす (fail-closed)。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return isTTYDevice(uint64(st.Rdev))
}

// isTTYDevice は、文字デバイス番号 rdev (Linux の dev_t) が、端末か (Documentation/admin-guide/devices.txt の割り当て)。
func isTTYDevice(rdev uint64) bool {
	major := (rdev>>8)&0xfff | (rdev>>32)&^uint64(0xfff)
	switch {
	case major == 2 || major == 3 || major == 4 || major == 5: // BSD pty・tty・/dev/tty・/dev/console・/dev/ptmx
		return true
	case 136 <= major && major <= 143: // Unix98 pty の slave (/dev/pts/N)
		return true
	case major == 166 || major == 188 || major == 204 || major == 205: // ttyACM・ttyUSB・シリアル
		return true
	}
	return false
}

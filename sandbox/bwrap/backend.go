package bwrap

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nananek/goronation/core/sandbox"
	"github.com/nananek/goronation/sandbox/contract"
)

// Backend は、bwrap を、サンドボックス契約 (core/sandbox) の実装にする薄いアダプタ。契約の Spec を、この package の Spec に翻訳し、
// 起動と検証は、Start と Argv (既存の API) に任せる。翻訳の前に、契約の共通検証 (sandbox/contract) も通す (両方を通らなければ、起動しない)。
type Backend struct {
	host  contract.Host
	rules contract.Rules
}

var _ sandbox.Backend = (*Backend)(nil)

// New は、ホストの状態 host (contract.CurrentHost) を持つ Backend を作る。
func New(host contract.Host) *Backend {
	b := &Backend{host: host}
	b.rules = contract.Rules{Host: host, Policy: contract.LinuxPolicy(), Caps: b.Capabilities()}
	return b
}

// Name は、"bwrap" を返す。
func (b *Backend) Name() string { return "bwrap" }

// Capabilities は、bwrap の宣言: path の付け替え (mount)・檻専用の loopback (netns)・子孫の回収 (pid namespace の init)・
// ホストのプロセスが見えない (pid namespace)。bwrap が足す環境変数は、PWD (--chdir があるとき)。
func (b *Backend) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{PathRemap: true, PrivateLoopback: true, KillsDescendants: true, PrivatePIDs: true, ExtraEnv: []string{"PWD"}}
}

// Probe は、bwrap が固定パスに在り、檻の中で /usr/bin/true を実行できるかを確かめる。無ければ ErrNotInstalled、使えなければ ErrUnusable を包んだ error
// (Linux 以外は、常に ErrNotInstalled)。起動は、Start (検証つき) を通し、Probe だけ別の引数を組まない。
func (b *Backend) Probe(ctx context.Context) error { return b.probe(ctx) }

// Start は、s を、契約の検証 (Validate・Resolve)・この package の検証 (Argv・symlink を辿った実体) の全部に通し、bwrap で起動する。
// どれかに通らなければ起動せず、ErrRejected を、端末への注入を塞げると確かめられなければ ErrTerminalUnsafe を包んだ error を返す。
func (b *Backend) Start(ctx context.Context, s sandbox.Spec) (sandbox.Cage, error) {
	return b.start(ctx, s)
}

// Prepare は、s を検証し、bwrap の argv に翻訳する (何も起動せず、ファイルシステムも見ない)。
func (b *Backend) Prepare(s sandbox.Spec) (sandbox.Prepared, error) {
	if err := b.rules.Validate(s); err != nil {
		return sandbox.Prepared{}, err
	}
	argv, err := b.translate(s).Argv()
	if err != nil {
		return sandbox.Prepared{}, fmt.Errorf("%w: %v", sandbox.ErrRejected, err)
	}
	return sandbox.Prepared{Argv: argv}, nil
}

// translate は、検証済みの契約の Spec s を、この package の Spec にする。mount は、System (/usr と symlink)・Read・Write の順で、
// 内側の path が、外側の path より前に来ないように並べる (先に作ると、後から外側を bind したときに隠れて、ro のはずのものが書ける)。
func (b *Backend) translate(s sandbox.Spec) Spec {
	bs := Spec{
		Host:       Host{Home: b.host.Home, Secrets: slices.Clone(b.host.Secrets)},
		Tmpfs:      slices.Clone(s.Scratch),
		Chdir:      s.Dir,
		Cmd:        append([]string{s.Exec}, s.Args...),
		NewSession: !s.Terminal,
		Stdin:      s.Stdin,
		Stdout:     s.Stdout,
		Stderr:     s.Stderr,
	}
	var binds []Bind
	if s.System {
		bs.Symlinks = []Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		}
		binds = append(binds, Bind{Src: "/usr", Dst: "/usr"})
	}
	for _, m := range s.Read {
		binds = append(binds, Bind{Src: m.HostPath, Dst: m.Guest(), InHome: m.InHome})
	}
	for _, m := range s.Write {
		binds = append(binds, Bind{Src: m.HostPath, Dst: m.Guest(), RW: true, InHome: m.InHome})
	}
	bs.Binds = ancestorsFirst(binds)
	for _, e := range s.Env {
		bs.Env = append(bs.Env, EnvVar{Key: e.Key, Value: e.Value})
	}
	return bs
}

// ancestorsFirst は、binds を、ある bind の下にある bind が、その bind より前に来ないように並べ直す (元の順を、できるだけ保つ)。
func ancestorsFirst(binds []Bind) []Bind {
	var out []Bind
	for _, b := range binds {
		at := len(out)
		for i, o := range out {
			if strings.HasPrefix(o.Dst, b.Dst+"/") { // o は b の下: b を o の前に置く
				at = i
				break
			}
		}
		out = slices.Insert(out, at, b)
	}
	return out
}

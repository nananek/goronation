//go:build linux

package main

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// jailProxyAddr は、檻の中で goro init が待ち受けるアドレス (檻の loopback。ホストの loopback とは別)。
const jailProxyAddr = "127.0.0.1:3128"

// cageConfig は、goro run の檻 1 つに見せるものと、その中で実行するもの。path はどれもホストの絶対 path。
//
// 檻の中の path は、ホストの path と同じ (identity): bind 元と bind 先は同じ path で、檻専用の固定の path (/work・/home/goro・
// /run/goro・/opt/...) は無い。エージェントの HOME・作業ディレクトリ・run dir・実行ファイル・goro 自身は、ホストの path のまま見える
// (mount の付け替えができない環境 (macOS の seatbelt) でも、同じ形で作れるようにするため。付け替えは、bwrap 固有の手段)。
// 副作用: 檻から、ホストのユーザー名と状態ディレクトリの path が見える (中身は見えない: 見えるのは、bind した path までの、空の
// ディレクトリの鎖だけ)。作業ディレクトリは、セッションごとに違う path になる。
type cageConfig struct {
	// Host は、bwrap.Spec の検証に使う、ホストの状態 (bwrap.CurrentHost)。
	Host bwrap.Host
	// Agent は、檻の中で動かすエージェント。
	Agent agentProfile
	// AgentExe・GoroExe は、エージェントと goro 自身の実体 (symlink を辿ったもの)。同じ path で、ro で見せる。
	AgentExe, GoroExe string
	// CACerts は、ホストの /etc/ssl/certs (無ければ空)。ro で見せる。
	CACerts string
	// RunDir は、egress の UDS (proxySockName) を置くディレクトリ。同じ path で、ディレクトリごと、ro で見せる。
	RunDir string
	// AgentHome は、エージェント専用の HOME (ログイン状態が残る)。同じ path で、rw で見せる。檻の HOME 環境変数も、この path。
	AgentHome string
	// Work は、作業ディレクトリ (セッションの clone か、ログイン用の空のディレクトリ)。同じ path で、rw で見せ、檻の cwd にする。
	Work string
	// Term は、ホストの TERM。
	Term string
	// Args は、エージェントへの引数。
	Args []string
}

// cageSpec は、c の檻の Spec を作る。標準入出力は、呼び手が足す。
//
// 檻に入るのは、ここに書いたものだけ: /usr (ro) とその symlink・証明書 (ro)・エージェントと goro の実体 (ro)・egress の UDS の
// ディレクトリ (ro)・エージェントの HOME と作業ディレクトリ (rw)。どれも、ホストと同じ path。ホストの HOME・~/.ssh・~/.claude・
// 環境変数は入らない。ネットワークは無く (bwrap が --unshare-all)、goro init が、loopback の TCP を UDS へ中継する。
func cageSpec(c cageConfig) bwrap.Spec {
	binds := []bwrap.Bind{c.bind("/usr", false)}
	if c.CACerts != "" {
		binds = append(binds, c.bind(c.CACerts, false))
	}
	binds = append(binds,
		c.bind(c.AgentExe, false),
		c.bind(c.GoroExe, false),
		c.bind(c.RunDir, false),
		c.bind(c.AgentHome, true),
		c.bind(c.Work, true),
	)
	// --no-forward-tty: 端末のシグナルは、エージェントが直接受ける。init が転送すると、二重に届く (Ctrl-C が 2 回になる)。
	cmd := []string{c.GoroExe, "init", "--listen", jailProxyAddr, "--upstream", filepath.Join(c.RunDir, proxySockName), "--no-forward-tty", "--", c.AgentExe}
	return bwrap.Spec{
		Host: c.Host,
		Symlinks: []bwrap.Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		Tmpfs: []string{"/tmp"},
		Binds: ancestorsFirst(binds),
		Env:   cageEnv(c.Agent, c.AgentHome, c.Term),
		Chdir: c.Work,
		Cmd:   append(cmd, c.Args...),
		// NewSession は付けない: 端末のシグナル (Ctrl-C・リサイズ) は、フォアグラウンドの process group ごと、檻の中のエージェントにも届く。
	}
}

// bind は、ホストの path p を、檻の中の同じ path に見せる Bind。p がホストの HOME の下なら、InHome を明示する
// (clone・エージェントの HOME・run dir・利用者が置いたエージェントの実行ファイルなど、goro run が決めた path だけ。機密の path は bwrap が拒否する)。
func (c cageConfig) bind(p string, rw bool) bwrap.Bind {
	return bwrap.Bind{Src: p, Dst: p, RW: rw, InHome: strings.HasPrefix(p, c.Host.Home+"/")}
}

// ancestorsFirst は、binds を、ある bind の下にある bind が、その bind より前に来ないように並べ直す (元の順を、できるだけ保つ)。
// mount は並べた順に作るので、下の path の bind を先に作ると、後から上の path を bind したときに、隠れて消える。bind 先が
// ホストの path になると、エージェントの実行ファイルが、HOME や作業ディレクトリの下にある、という配置が起こりうる。
func ancestorsFirst(binds []bwrap.Bind) []bwrap.Bind {
	var out []bwrap.Bind
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

// termRE は、檻に渡す TERM の形。
var termRE = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// cageEnv は、檻の環境変数 (これだけ。ホストの環境変数は渡らない): 共通の HOME (エージェントの HOME の path)・PATH・TERM・LANG に、
// エージェントの p.env を足す。proxy の変数は、goro init が設定する。TERM は、形が正しいときだけ渡し、そうでなければ dumb にする。
func cageEnv(p agentProfile, home, term string) []bwrap.EnvVar {
	if !termRE.MatchString(term) {
		term = "dumb"
	}
	return append([]bwrap.EnvVar{
		{Key: "HOME", Value: home},
		{Key: "PATH", Value: "/usr/bin:/bin"},
		{Key: "TERM", Value: term},
		{Key: "LANG", Value: "C.UTF-8"},
	}, p.env...)
}

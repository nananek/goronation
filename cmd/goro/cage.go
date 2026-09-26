//go:build linux

package main

import (
	"regexp"
	"strings"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// 檻の中の path と、待ち受けるアドレス。エージェントの実行ファイルの path は、agentProfile.jailExe()。
const (
	jailGoro      = "/opt/goro/goro"
	jailRun       = "/run/goro"
	jailHome      = "/home/goro"
	jailWork      = "/work"
	jailProxyAddr = "127.0.0.1:3128"
)

// cageConfig は、goro run の檻 1 つに見せるものと、その中で実行するもの。path はどれもホストの絶対 path。
type cageConfig struct {
	// Host は、bwrap.Spec の検証に使う、ホストの状態 (bwrap.CurrentHost)。
	Host bwrap.Host
	// Agent は、檻の中で動かすエージェント。
	Agent agentProfile
	// AgentExe・GoroExe は、エージェント (Agent.jailExe() に見せる) と goro 自身の実体 (symlink を辿ったもの)。ro で見せる。
	AgentExe, GoroExe string
	// CACerts は、ホストの /etc/ssl/certs (無ければ空)。ro で見せる。
	CACerts string
	// RunDir は、egress の UDS (proxySockName) を置くディレクトリ。ディレクトリごと、ro で見せる。
	RunDir string
	// AgentHome は、エージェント専用の HOME (ログイン状態が残る)。rw で見せる。
	AgentHome string
	// Work は、/work に見せる作業ディレクトリ (セッションの clone か、ログイン用の空のディレクトリ)。rw で見せる。
	Work string
	// Term は、ホストの TERM。
	Term string
	// Args は、エージェントへの引数。
	Args []string
}

// cageSpec は、c の檻の Spec を作る。標準入出力は、呼び手が足す。
//
// 檻に入るのは、ここに書いたものだけ: /usr (ro) とその symlink・証明書 (ro)・エージェントと goro の実体 (ro)・egress の UDS の
// ディレクトリ (ro)・エージェントの HOME と作業ディレクトリ (rw)。ホストの HOME・~/.ssh・~/.claude・環境変数は入らない。
// ネットワークは無く (bwrap が --unshare-all)、goro init が、loopback の TCP を UDS へ中継する。
func cageSpec(c cageConfig) bwrap.Spec {
	binds := []bwrap.Bind{c.bind("/usr", "/usr", false)}
	if c.CACerts != "" {
		binds = append(binds, c.bind(c.CACerts, "/etc/ssl/certs", false))
	}
	binds = append(binds,
		c.bind(c.AgentExe, c.Agent.jailExe(), false),
		c.bind(c.GoroExe, jailGoro, false),
		c.bind(c.RunDir, jailRun, false),
		c.bind(c.AgentHome, jailHome, true),
		c.bind(c.Work, jailWork, true),
	)
	// --no-forward-tty: 端末のシグナルは、エージェントが直接受ける。init が転送すると、二重に届く (Ctrl-C が 2 回になる)。
	cmd := []string{jailGoro, "init", "--listen", jailProxyAddr, "--upstream", jailRun + "/" + proxySockName, "--no-forward-tty", "--", c.Agent.jailExe()}
	return bwrap.Spec{
		Host: c.Host,
		Symlinks: []bwrap.Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		Tmpfs: []string{"/tmp"},
		Binds: binds,
		Env:   cageEnv(c.Agent, c.Term),
		Chdir: jailWork,
		Cmd:   append(cmd, c.Args...),
		// NewSession は付けない: 端末のシグナル (Ctrl-C・リサイズ) は、フォアグラウンドの process group ごと、檻の中のエージェントにも届く。
	}
}

// bind は、ホストの path src を、檻の中の dst に見せる Bind。src がホストの HOME の下なら、InHome を明示する
// (clone・エージェントの HOME・run dir・利用者が置いたエージェントの実行ファイルなど、goro run が決めた path だけ。機密の path は bwrap が拒否する)。
func (c cageConfig) bind(src, dst string, rw bool) bwrap.Bind {
	return bwrap.Bind{Src: src, Dst: dst, RW: rw, InHome: strings.HasPrefix(src, c.Host.Home+"/")}
}

// termRE は、檻に渡す TERM の形。
var termRE = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// cageEnv は、檻の環境変数 (これだけ。ホストの環境変数は渡らない): 共通の HOME・PATH・TERM・LANG に、エージェントの p.env を足す。
// proxy の変数は、goro init が設定する。TERM は、形が正しいときだけ渡し、そうでなければ dumb にする。
func cageEnv(p agentProfile, term string) []bwrap.EnvVar {
	if !termRE.MatchString(term) {
		term = "dumb"
	}
	return append([]bwrap.EnvVar{
		{Key: "HOME", Value: jailHome},
		{Key: "PATH", Value: "/usr/bin:/bin"},
		{Key: "TERM", Value: term},
		{Key: "LANG", Value: "C.UTF-8"},
	}, p.env...)
}

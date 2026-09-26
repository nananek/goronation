//go:build linux

package main

import (
	"regexp"
	"strings"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// 檻の中の path と、待ち受けるアドレス。
const (
	jailClaude    = "/opt/claude/claude"
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
	// ClaudeExe・GoroExe は、claude と goro 自身の実体 (symlink を辿ったもの)。ro で見せる。
	ClaudeExe, GoroExe string
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
	// Args は、claude への引数。
	Args []string
}

// cageSpec は、c の檻の Spec を作る。標準入出力は、呼び手が足す。
//
// 檻に入るのは、ここに書いたものだけ: /usr (ro) とその symlink・証明書 (ro)・claude と goro の実体 (ro)・egress の UDS の
// ディレクトリ (ro)・エージェントの HOME と作業ディレクトリ (rw)。ホストの HOME・~/.ssh・~/.claude・環境変数は入らない。
// ネットワークは無く (bwrap が --unshare-all)、goro init が、loopback の TCP を UDS へ中継する。
func cageSpec(c cageConfig) bwrap.Spec {
	binds := []bwrap.Bind{c.bind("/usr", "/usr", false)}
	if c.CACerts != "" {
		binds = append(binds, c.bind(c.CACerts, "/etc/ssl/certs", false))
	}
	binds = append(binds,
		c.bind(c.ClaudeExe, jailClaude, false),
		c.bind(c.GoroExe, jailGoro, false),
		c.bind(c.RunDir, jailRun, false),
		c.bind(c.AgentHome, jailHome, true),
		c.bind(c.Work, jailWork, true),
	)
	// --no-forward-tty: 端末のシグナルは、claude が直接受ける。init が転送すると、二重に届く (Ctrl-C が 2 回になる)。
	cmd := []string{jailGoro, "init", "--listen", jailProxyAddr, "--upstream", jailRun + "/" + proxySockName, "--no-forward-tty", "--", jailClaude}
	return bwrap.Spec{
		Host: c.Host,
		Symlinks: []bwrap.Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		Tmpfs: []string{"/tmp"},
		Binds: binds,
		Env:   cageEnv(c.Term),
		Chdir: jailWork,
		Cmd:   append(cmd, c.Args...),
		// NewSession は付けない: 端末のシグナル (Ctrl-C・リサイズ) は、フォアグラウンドの process group ごと、檻の中の claude にも届く。
	}
}

// bind は、ホストの path src を、檻の中の dst に見せる Bind。src がホストの HOME の下なら、InHome を明示する
// (clone・エージェントの HOME・run dir・利用者が置いた claude など、goro run が決めた path だけ。機密の path は bwrap が拒否する)。
func (c cageConfig) bind(src, dst string, rw bool) bwrap.Bind {
	return bwrap.Bind{Src: src, Dst: dst, RW: rw, InHome: strings.HasPrefix(src, c.Host.Home+"/")}
}

// termRE は、檻に渡す TERM の形。
var termRE = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// cageEnv は、檻の環境変数 (これだけ。ホストの環境変数は渡らない)。proxy の変数は、goro init が設定する。
// TERM は、形が正しいときだけ渡し、そうでなければ dumb にする。
// 通信を止める変数 (許可した宛先以外への、必須でない通信が、拒否として終了後の一覧に出て、必要な宛先に見えるのを避ける):
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC・DISABLE_TELEMETRY・DISABLE_ERROR_REPORTING・DISABLE_AUTOUPDATER に加えて、
// CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL (公式のプラグイン marketplace の自動インストール。これが無いと、
// 対話起動のたびに downloads.claude.ai:443 と github.com:443 が拒否される。実測: この変数だけで、両方の拒否が消える)。
func cageEnv(term string) []bwrap.EnvVar {
	if !termRE.MatchString(term) {
		term = "dumb"
	}
	return []bwrap.EnvVar{
		{Key: "HOME", Value: jailHome},
		{Key: "PATH", Value: "/usr/bin:/bin"},
		{Key: "TERM", Value: term},
		{Key: "LANG", Value: "C.UTF-8"},
		{Key: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Value: "1"},
		{Key: "DISABLE_TELEMETRY", Value: "1"},
		{Key: "DISABLE_ERROR_REPORTING", Value: "1"},
		{Key: "DISABLE_AUTOUPDATER", Value: "1"},
		{Key: "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", Value: "1"},
	}
}

//go:build linux

package main

import (
	"io"
	"strings"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// 檻の中の path と、待ち受けるアドレス。cmd/goronation/cage.go の命名を踏まえるが、別バイナリなので
// 独立 (s1-plan の「共有せず独立実装する」方針どおり)。
const (
	jailGoro      = "/opt/goronation/goronation"
	jailAgentDir  = "/opt/agent"
	jailHome      = "/home/framecapture"
	jailWork      = "/work"
	jailRun       = "/run/framecapture"
	jailCACert    = "/framecapture-ca.pem"
	jailProxyAddr = "127.0.0.1:3128"
	proxySockName = "proxy.sock"
)

// cageConfig は、cmd/framecapture の檻 1 つに見せるものと、その中で実行するもの。path はどれもホストの絶対 path。
// goronation run の cageConfig と違い、push・MCP・PTY・セッション管理・認証情報の永続化は無い (フレーム
// 採取に要らない)。
type cageConfig struct {
	// Host は、bwrap.Spec の検証に使う、ホストの状態 (bwrap.CurrentHost)。
	Host bwrap.Host
	// AgentExe は、檻の中で動かすエージェント (claude・opencode) の実行ファイル (ホストの絶対 path、
	// symlink 解決済み)。
	AgentExe string
	// AgentBinName は、AgentExe を檻の中に置くときのファイル名 (jailAgentDir の下)。
	AgentBinName string
	// GoroExe は、goronation 自身の実行ファイル (init サブコマンドの起動に使う)。
	GoroExe string
	// CACertPEM は、fake サーバーの自己署名 CA 証明書 (PEM) を書いたホストの一時ファイル。
	CACertPEM string
	// RunDir は、connectproxy の UDS (proxySockName) を置くディレクトリ。ディレクトリごと、ro で見せる。
	RunDir string
	// Home は、空の HOME (rw で見せる。認証情報・会話の履歴は共有しない: 1 回限りの採取用)。
	Home string
	// Work は、/work に見せる作業ディレクトリ (opencode.json などを置く)。rw で見せる。
	Work string
	// Env は、エージェント固有の環境変数 (共通の HOME・PATH・TERM・LANG・NODE_EXTRA_CA_CERTS に追加する)。
	Env []bwrap.EnvVar
	// Args は、エージェントへの引数。
	Args []string
	// Stdout・Stderr は、檻のコマンドの標準出力・エラー出力。
	Stdout, Stderr io.Writer
}

// jailAgentExe は、檻の中でのエージェントの実行ファイルの path。
func (c cageConfig) jailAgentExe() string { return jailAgentDir + "/" + c.AgentBinName }

// cageSpec は、c の檻の Spec を作る。
//
// 檻に入るのは、ここに書いたものだけ: /usr (ro) とその symlink・エージェントと goronation の実体 (ro)・
// 自己署名 CA (ro)・connectproxy の UDS のディレクトリ (ro)・空の HOME・作業ディレクトリ (rw)。ホストの
// HOME・~/.ssh・~/.claude・環境変数は入らない。ネットワークは無く (bwrap が --unshare-all)、
// goronation init が、loopback の TCP を connectproxy の UDS へ中継する。
func cageSpec(c cageConfig) bwrap.Spec {
	binds := []bwrap.Bind{
		c.bind("/usr", "/usr", false),
		c.bind(c.AgentExe, c.jailAgentExe(), false),
		c.bind(c.GoroExe, jailGoro, false),
		c.bind(c.CACertPEM, jailCACert, false),
		c.bind(c.RunDir, jailRun, false),
		c.bind(c.Home, jailHome, true),
		c.bind(c.Work, jailWork, true),
	}
	cmd := append([]string{
		jailGoro, "init", "--listen", jailProxyAddr, "--upstream", jailRun + "/" + proxySockName,
		"--no-forward-tty", "--", c.jailAgentExe(),
	}, c.Args...)

	return bwrap.Spec{
		Host: c.Host,
		Symlinks: []bwrap.Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		Tmpfs: []string{"/tmp"},
		Binds: binds,
		Env:   cageEnv(c.Env),
		Chdir: jailWork,
		Cmd:   cmd,
		// NewSession は付けない: このハーネスは対話端末に直結しない (標準入出力は pipe)。
		NewSession: false,
		Stdout:     c.Stdout,
		Stderr:     c.Stderr,
	}
}

// bind は、ホストの path src を、檻の中の dst に見せる Bind。src がホストの HOME の下なら、InHome を
// 明示する (goronation run の cage.go と同じ理屈)。
func (c cageConfig) bind(src, dst string, rw bool) bwrap.Bind {
	return bwrap.Bind{Src: src, Dst: dst, RW: rw, InHome: strings.HasPrefix(src, c.Host.Home+"/")}
}

// cageEnv は、檻の環境変数 (これだけ。ホストの環境変数は渡らない): 共通の HOME・PATH・TERM・LANG・
// NODE_EXTRA_CA_CERTS (fake サーバーの自己署名 CA を、Node.js/Bun ランタイムに信頼させる) に、
// エージェント固有の extra を足す。proxy の変数は、goronation init が設定する。
func cageEnv(extra []bwrap.EnvVar) []bwrap.EnvVar {
	env := []bwrap.EnvVar{
		{Key: "HOME", Value: jailHome},
		{Key: "PATH", Value: "/usr/bin:/bin"},
		{Key: "TERM", Value: "dumb"},
		{Key: "LANG", Value: "C.UTF-8"},
		{Key: "NODE_EXTRA_CA_CERTS", Value: jailCACert},
	}
	return append(env, extra...)
}

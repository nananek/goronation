//go:build linux

package main

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// 檻の中の path と、待ち受けるアドレス。エージェントの実行ファイルの path は、agentProfile.jailExe()。
const (
	jailGoro      = "/opt/goronation/goronation"
	jailRun       = "/run/goronation"
	jailHome      = "/home/goronation"
	jailAuth      = "/auth"
	jailWork      = "/work"
	jailProxyAddr = "127.0.0.1:3128"
)

// jailGitBase は、--push のとき、檻の git が https://github.com/ の代わりに使う URL の base
// (goronation init が中継する loopback + egress/git の受け口)。CONNECT を経由しない、直接の宛先。
const jailGitBase = "http://" + jailProxyAddr + git.PathPrefix

// cageConfig は、goronation run の檻 1 つに見せるものと、その中で実行するもの。path はどれもホストの絶対 path。
type cageConfig struct {
	// Host は、bwrap.Spec の検証に使う、ホストの状態 (bwrap.CurrentHost)。
	Host bwrap.Host
	// Agent は、檻の中で動かすエージェント。
	Agent agentProfile
	// AgentExe・GoroExe は、エージェント (Agent.jailExe() に見せる) と goronation 自身の実体 (symlink を辿ったもの)。ro で見せる。
	AgentExe, GoroExe string
	// CACerts は、ホストの /etc/ssl/certs (無ければ空)。ro で見せる。
	CACerts string
	// RunDir は、egress の UDS (proxySockName) を置くディレクトリ。ディレクトリごと、ro で見せる。
	RunDir string
	// AgentHome は、repo ごとの (--login では、ログイン用の) HOME。会話の履歴・メモリ・trust の承認が残る。rw で見せる。
	AgentHome string
	// AuthDir は、エージェントの認証情報の置き場 (ログイン状態)。全 repo の檻に共通で、jailAuth に rw で見せる。
	AuthDir string
	// Work は、/work に見せる作業ディレクトリ (セッションの clone か、ログイン用の空のディレクトリ)。rw で見せる。
	Work string
	// Term は、ホストの TERM。
	Term string
	// TZ は、檻に渡すタイムゾーン (tz.go の hostTZ が作る。空なら渡さない: これまでどおり UTC)。
	TZ string
	// Args は、エージェントへの引数。
	Args []string
	// PushRepo は、--push owner/repo が指定されたときの repo ("owner/repo"。空なら無効)。指定されていれば、
	// 檻の git が https://github.com/ の代わりに egress/git の受け口 (jailGitBase) を使うよう、GIT_CONFIG_*
	// 環境変数で insteadOf を設定し、GORONATION_PUSH_REPO・GORONATION_PUSH_REF_PREFIX で、檻の中に repo と許可された
	// ref の名前空間を伝える (goronation pr create が GORONATION_PUSH_REPO を読む)。
	PushRepo string
	// PushRefPrefix は、PushRepo が指定されたときの、許可された ref の接頭辞 (git.Policy.Prefix の値。
	// "refs/heads/goronation/<セッション>/")。
	PushRefPrefix string
	// MCPServers は、Agent に登録する MCP サーバー (mcp.go の mcpServersFor が作る。--push が無効なら空)。
	// Agent.mcp が nil のエージェントには、何も注入しない。
	MCPServers []mcpServerDef
	// PTY は、Stdin・Stdout・Stderr が、goronation run 専用の pty の slave であることを示す (呼び手 (runCage) が、
	// ホストの実端末に直結できるときだけ true にする)。true なら、bwrap を --new-session (NewSession) で起動し
	// (TIOCSTI の確認を要らなくする)、goronation init に --set-ctty を渡す: goronation init が、エージェントを起動すると
	// き、エージェント自身が新しいセッションの leader になり、渡された pty を自分の制御端末にする。false
	// (既定) なら、これまでどおり、ホストの端末に直結する。
	PTY bool
	// NonDumpable は、goronation init を PID 1 (bwrap --as-pid-1。bwrap 自身の PID 1 を挟まない) にして、--non-dumpable を渡す
	// (chat セッション: エージェントの標準入出力の socket を持つ、init の fd を、檻の中の同じ uid のプロセスに、pidfd_getfd・
	// ptrace・/proc/<pid>/mem で奪わせない)。エージェント自身も dumpable=0 にする方法は、chat_exe_linux.go。
	NonDumpable bool
	// RelayPort は、0 でなければ、goronation init に --relay-control --relay-port を渡す (要 NonDumpable。ADR 0019・0023): init の
	// 標準入力の socketpair が、ホストが要求ごとに fd を送る control になり、要求は 127.0.0.1:RelayPort の上流に中継される。
	RelayPort int
}

// cageSpec は、c の檻の Spec を作る。標準入出力は、呼び手が足す。
//
// 檻に入るのは、ここに書いたものだけ: /usr (ro) とその symlink・証明書 (ro)・エージェントと goronation の実体 (ro)・egress の UDS の
// ディレクトリ (ro)・repo ごとの HOME と認証用ディレクトリと作業ディレクトリ (rw)。ホストの HOME・~/.ssh・~/.claude・環境変数は入らない。
// 別の repo の HOME は、入らない。
// ネットワークは無く (bwrap が --unshare-all)、goronation init が、loopback の TCP を UDS へ中継する。
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
		c.bind(c.AuthDir, jailAuth, true),
		c.bind(c.Work, jailWork, true),
	)
	// --no-forward-tty: 端末のシグナルは、エージェントが直接受ける (PTY のときは、専用の pty の、PTY でないときは
	// ホストの実端末の、フォアグラウンドの process group として)。init が転送すると、二重に届く (Ctrl-C が 2 回になる)。
	cmd := []string{jailGoro, "init", "--listen", jailProxyAddr, "--upstream", jailRun + "/" + proxySockName, "--no-forward-tty"}
	if c.PTY {
		cmd = append(cmd, "--set-ctty")
	}
	if c.NonDumpable {
		cmd = append(cmd, "--non-dumpable")
	}
	if c.RelayPort != 0 {
		cmd = append(cmd, "--relay-control", "--relay-port", strconv.Itoa(c.RelayPort))
	}
	cmd = append(cmd, "--", c.Agent.jailExe())
	// MCP サーバー (goronation mcp) の登録: エージェントごとの変換 (Agent.mcp) が、起動時の引数・環境変数のどちらに
	// するかを決める (claude は引数、opencode は環境変数。cageSpec は、その違いを知らない)。
	var mcpArgs []string
	var mcpEnv []bwrap.EnvVar
	if len(c.MCPServers) > 0 && c.Agent.mcp != nil {
		mcpArgs, mcpEnv = c.Agent.mcp(c.MCPServers)
	}
	cmd = append(cmd, mcpArgs...)
	env := append(cageEnv(c.Agent, c.Term, c.TZ, c.PushRepo, c.PushRefPrefix), mcpEnv...)
	return bwrap.Spec{
		Host: c.Host,
		Symlinks: []bwrap.Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		Tmpfs: []string{"/tmp"},
		Binds: binds,
		Env:   env,
		Chdir: jailWork,
		Cmd:   append(cmd, c.Args...),
		// NewSession は、c.PTY のときだけ付ける: 専用の pty の slave を、goronation init が (--set-ctty で) 自分の
		// 制御端末にできるよう、まず制御端末の無い新しいセッションにする。c.PTY でないとき (ホストの実端末に
		// 直結するとき) は付けない: 端末のシグナル (Ctrl-C・リサイズ) が、フォアグラウンドの process group
		// ごと、檻の中のエージェントにも届く、これまでの形のまま。
		NewSession: c.PTY,
		AsPID1:     c.NonDumpable,
	}
}

// bind は、ホストの path src を、檻の中の dst に見せる Bind。src がホストの HOME の下なら、InHome を明示する
// (clone・エージェントの HOME・run dir・利用者が置いたエージェントの実行ファイルなど、goronation run が決めた path だけ。機密の path は bwrap が拒否する)。
func (c cageConfig) bind(src, dst string, rw bool) bwrap.Bind {
	return bwrap.Bind{Src: src, Dst: dst, RW: rw, InHome: strings.HasPrefix(src, c.Host.Home+"/")}
}

// termRE は、檻に渡す TERM の形。
var termRE = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// cageEnv は、檻の環境変数 (これだけ。ホストの環境変数は渡らない): 共通の HOME・PATH・TERM・LANG に、エージェントの p.env と、
// 認証用ディレクトリの場所を教える p.creds.env を足す。tz が空でなければ TZ も足す (--push の有無に関わらず、常に)。
// push・refPrefix が空でなければ、--push の配線 (下の pushEnv) も足す。proxy の変数は、goronation init が設定する。
// TERM は、形が正しいときだけ渡し、そうでなければ dumb にする。
func cageEnv(p agentProfile, term, tz, push, refPrefix string) []bwrap.EnvVar {
	if !termRE.MatchString(term) {
		term = "dumb"
	}
	env := append([]bwrap.EnvVar{
		{Key: "HOME", Value: jailHome},
		{Key: "PATH", Value: "/usr/bin:/bin"},
		{Key: "TERM", Value: term},
		{Key: "LANG", Value: "C.UTF-8"},
	}, p.env...)
	env = append(env, p.creds.env...)
	if tz != "" {
		env = append(env, bwrap.EnvVar{Key: "TZ", Value: tz})
	}
	if push != "" {
		env = append(env, pushEnv(push, refPrefix)...)
	}
	return env
}

// pushEnv は、--push owner/repo の環境変数: GORONATION_PUSH_REPO・GORONATION_PUSH_REF_PREFIX (goronation pr create と、
// エージェントへの手がかり) と、git の https://github.com/ を jailGitBase に付け替える insteadOf
// (GIT_CONFIG_COUNT・GIT_CONFIG_KEY_0・GIT_CONFIG_VALUE_0。git 2.31 以降が読む環境変数によるコマンド設定。
// ファイルを書かずに、この起動限りの設定にできる)。
func pushEnv(repo, refPrefix string) []bwrap.EnvVar {
	return []bwrap.EnvVar{
		{Key: "GORONATION_PUSH_REPO", Value: repo},
		{Key: "GORONATION_PUSH_REF_PREFIX", Value: refPrefix},
		{Key: "GIT_CONFIG_COUNT", Value: "1"},
		{Key: "GIT_CONFIG_KEY_0", Value: "url." + jailGitBase + ".insteadOf"},
		{Key: "GIT_CONFIG_VALUE_0", Value: "https://github.com/"},
	}
}

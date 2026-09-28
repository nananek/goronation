//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/nananek/goronation/cmd/internal/credfile"
	"github.com/nananek/goronation/cmd/internal/session"
	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// runUsage は、goronation run -h の使い方。エージェントごとの記述 (既定の許可宛先・実行ファイルを指す環境変数・--login・終了操作) は、
// agents の表 (agentProfile) から作る: エージェントを足しても、ここは変えない。
func runUsage() string {
	var b strings.Builder
	def := defaultAgent()
	fmt.Fprintf(&b, "使い方: goronation run (--repo PATH | --session ID | --login) [--agent NAME] [オプション] [-- エージェントへの引数...]\n\n")
	fmt.Fprintf(&b, "エージェント (%s) を、檻 (ネットワークの無い bwrap) の中で、ホストの repo の private clone の上で動かす。\n", agentNames())
	b.WriteString("ホストの作業ツリー・.git・~/.ssh・エージェントの設定と認証情報 (~/.claude など)・環境変数は、檻から見えない。clone されるのはコミット済みの内容だけ。\n\n")
	fmt.Fprintf(&b, "  --agent NAME      動かすエージェント: %s (省略は %s)。ログイン状態はエージェントごとに 1 つ (全 repo で共有)、会話の履歴・メモリ・trust の承認は repo ごとに別\n", agentNames(), def.name)
	b.WriteString(`  --repo PATH       PATH (ローカルの repo) の private clone を作り、その中でエージェントを起動する
  --session ID      前の goronation run のセッションを再開する (同じ clone と HOME が見える)。エージェントは、そのセッションを作ったもの (--agent は省略できる。別のエージェントは断る)
  --login           repo・clone 無しで、空の作業ディレクトリでエージェントのログインを行う (ログイン状態は、認証情報のディレクトリに残り、全 repo の檻で使う)。エージェントごとの起動は、下の「エージェント」
                    ログインは、エージェント自身の画面で行う。goronation は出力を解釈しない (端末に直結する)。
  --name N          clone の user.name (--repo のとき。既定は goronation)
  --email E         clone の user.email (--repo のとき)
  --state-dir DIR   状態を置く場所 (既定は $XDG_STATE_HOME/goronation か ~/.local/state/goronation)。セッションは <DIR>/sessions/、エージェントごとの認証情報 (auth/)・repo ごとの HOME (homes/)・ログイン用のディレクトリは <DIR>/agents/<エージェント名>/
  --bin PATH        動かすエージェントの実行ファイル (既定は、環境変数 GORO_<エージェント名の大文字> か、PATH の実行ファイル。下の「エージェント」)
  --allow HOST:PORT 檻から届く宛先を足す (何度でも書ける。既定は、エージェントごと。下の「エージェント」)
  --push OWNER/REPO 檻の git push・fetch と goronation pr create (檻の中のコマンド) を、この 1 つの repo だけに許す
                    (トークンは檻に渡さない)。--login とは併用できない。ref は refs/heads/goronation/<セッション ID>/
                    の下だけ (GORO_PUSH_REF_PREFIX で檻に伝える)。ready for review にするのは、ホストの goronation pr ready
  -- ARGS...        エージェントに渡す引数 (--login のときは、そのエージェントのログインの引数の後ろに付く)

エージェント:
`)
	for _, p := range agents {
		title := p.name
		if p.name == def.name {
			title += " (既定)"
		}
		fmt.Fprintf(&b, "  %s\n", title)
		fmt.Fprintf(&b, "      実行ファイル: 環境変数 %s か、PATH の %s\n", p.exeEnv(), p.binName())
		fmt.Fprintf(&b, "      既定の許可宛先: %s\n", strings.Join(p.hosts(), " "))
		fmt.Fprintf(&b, "      --login: %s\n", p.loginUsage)
		fmt.Fprintf(&b, "      終了: %s\n", p.exitHint)
		if p.resumeUsage != "" {
			fmt.Fprintf(&b, "      続き: %s\n", p.resumeUsage)
		}
	}
	fmt.Fprintf(&b, `
例:
  goronation run --login                          既定のエージェント (%s) のログイン (別のエージェントは、--agent NAME を足す)
  goronation run --repo ~/work/foo                foo の private clone の中で、既定のエージェントと対話する
  goronation run --agent NAME --repo ~/work/foo   同じことを、エージェントを選んで行う
  goronation run --session ID -- ARGS...          再開する (エージェントは、そのセッションを作ったもの。-- の後ろはエージェントへの引数)
  goronation export ID                            成果 (コミット) を bundle にして、取り込みのコマンドを表示する

限界: 檻からホストの localhost には届かない。ローカルのモデルサーバー (Ollama・LM Studio など) は使えない。

起動後の Ctrl-C は、エージェントの中断として効く (goronation run 自身は終了しない)。止めるときは、エージェントの終了操作 (上の「終了」) か、別の端末から goronation run に SIGTERM。
終わると、セッション ID・再開と取り出しのコマンド・拒否された宛先を表示する。
`, def.name)
	return b.String()
}

// removedFlags は、廃止したオプションの名前 (使うと、--bin を教える error にする)。エージェントごとの実行ファイルのオプション
// (--claude) は、エージェントが増えるたびにオプションが増えるので、--bin 1 つにした。
var removedFlags = []string{"claude"}

// runOptions は、goronation run の引数。
type runOptions struct {
	repo, session string
	login         bool
	name, email   string
	stateDir      string // 空なら既定
	agent         string // 動かすエージェント (agents の表の name)。空は、--session なら記録のエージェント、それ以外は既定
	bin           string // 動かすエージェントの実行ファイル。空なら GORO_<NAME> か PATH
	allow         []string
	push          string   // "owner/repo"。空なら無効 (--login とは併用できない)
	agentArgs     []string // エージェントへの引数 (-- の後ろ)
}

// stringList は、何度でも書ける文字列のオプション。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// parseRunArgs は goronation run の引数を解釈する。不正なら、理由と使い方を stderr に出して error を返す。
// -h のときは、使い方を出して flag.ErrHelp を返す。
func parseRunArgs(args []string, stderr io.Writer) (runOptions, error) {
	var o runOptions
	head, tail := args, []string(nil)
	if i := indexOf(args, "--"); i >= 0 {
		head, tail = args[:i], args[i+1:]
	}
	flags := flag.NewFlagSet("goronation run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {} // 使い方は、-h のときだけ (下)。エラーには、-h の案内 1 行だけを添える
	var allow stringList
	flags.StringVar(&o.repo, "repo", "", "")
	flags.StringVar(&o.session, "session", "", "")
	flags.BoolVar(&o.login, "login", false, "")
	flags.StringVar(&o.name, "name", "", "")
	flags.StringVar(&o.email, "email", "", "")
	flags.StringVar(&o.stateDir, "state-dir", "", "")
	flags.StringVar(&o.agent, "agent", "", "") // 空 = 省略
	flags.StringVar(&o.bin, "bin", "", "")
	flags.StringVar(&o.push, "push", "", "")
	for _, name := range removedFlags {
		flags.Func(name, "", func(string) error {
			return fmt.Errorf("廃止した。--bin PATH を使う (環境変数 %s は、そのまま使える)", exeEnvName(name))
		})
	}
	flags.Var(&allow, "allow", "")
	if err := flags.Parse(head); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, runUsage())
		} else {
			fmt.Fprintln(stderr, "使い方: goronation run -h") // flag が、理由を出している
		}
		return o, err
	}
	o.allow, o.agentArgs = allow, tail
	agentSet := false // --agent が書かれたか (省略と、空の値を区別する)
	flags.Visit(func(f *flag.Flag) { agentSet = agentSet || f.Name == "agent" })

	fail := func(format string, a ...any) (runOptions, error) {
		fmt.Fprintf(stderr, "goronation run: "+format+"\n", a...)
		fmt.Fprintln(stderr, "使い方: goronation run -h")
		return o, errors.New("引数が不正")
	}
	if flags.NArg() > 0 {
		return fail("余計な引数 %q (エージェントへの引数は -- の後ろに書く)", flags.Arg(0))
	}
	modes := 0
	for _, set := range []bool{o.repo != "", o.session != "", o.login} {
		if set {
			modes++
		}
	}
	if modes != 1 {
		return fail("--repo・--session・--login のうち、ちょうど 1 つが要る")
	}
	if (o.name != "" || o.email != "") && o.repo == "" {
		return fail("--name と --email は、--repo のときだけ使える")
	}
	if o.push != "" {
		if o.login {
			return fail("--push は --login と一緒には使えない (push にはセッションが要る)")
		}
		if _, err := git.ParseRepo(o.push); err != nil {
			return fail("--push は owner/repo の形: %q", o.push)
		}
	}
	switch {
	case agentSet:
		if _, ok := agentByName(o.agent); !ok {
			return fail("--agent は %s: %q", agentNames(), o.agent)
		}
	case o.session == "":
		o.agent = defaultAgent().name // 既定。--session で省略したときは、空のまま: セッションを作ったエージェントで動かす (doRun が決める)
	}
	if err := checkAllow(o.allow); err != nil {
		return fail("%v", err)
	}
	return o, nil
}

// profile は、o のエージェントの profile。--agent の値は、parseRunArgs が確かめてある (空は既定のエージェント)。
func (o runOptions) profile() agentProfile {
	if p, ok := agentByName(o.agent); ok {
		return p
	}
	return defaultAgent()
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// resolveExe は、実行ファイル p を、symlink を辿った絶対 path にする。通常のファイルで、実行できなければ error。
func resolveExe(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s は、実行できる通常のファイルではない", real)
	}
	return real, nil
}

// resolveAgentExe は、檻に見せるエージェント p の実体を決める: --bin、なければ環境変数 (GORO_<NAME>)、
// なければ PATH の実行ファイル (binName)。どれも、symlink を辿った実体にする (claude の native 版は、~/.local/bin/claude が、版ごとの実体への
// symlink)。スクリプト (先頭が #!) は、檻の中で、呼ぶ先の実体が見えず動かないので断る。
func resolveAgentExe(p agentProfile, flagVal, envVal string, lookPath func(string) (string, error)) (string, error) {
	path := flagVal
	if path == "" {
		path = envVal
	}
	if path == "" {
		found, err := lookPath(p.binName())
		if err != nil { // err の中身 (exec: "...": executable file not found in $PATH) は、案内と同じことなので、出さない
			return "", fmt.Errorf("%s が見つからない。PATH に置くか、--bin PATH か %s で指す", p.binName(), p.exeEnv())
		}
		path = found
	}
	real, err := resolveExe(path)
	if err != nil {
		return "", fmt.Errorf("%s (%s) を使えない: %w", p.name, path, err)
	}
	if isScript(real) {
		return "", fmt.Errorf("%s (%s) はスクリプトで、檻の中では動かない。実体を --bin PATH か %s で指す (例: %s)", p.name, real, p.exeEnv(), p.exeExample)
	}
	return real, nil
}

// isScript は、path のファイルの先頭が #! か (読めなければ false。実行できるかは、resolveExe が確かめた)。
func isScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [2]byte // 短いファイルは、残りが 0 のままで、#! に一致しない
	_, _ = io.ReadFull(f, head[:])
	return head[0] == '#' && head[1] == '!'
}

// ensureDir は、自分が使う dir を 0700 で作る (あれば、権限を 0700 にする)。
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// runRun は goronation run の本体で、終了コードを返す (claude の終了コード。goronation run 自身の失敗は 1、引数の不正は 2)。
func runRun(args []string, stderr io.Writer) int {
	o, err := parseRunArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := watchSignals(cancel)
	defer sw.stop()

	code := doRun(ctx, o, sw, stderr)
	if s := sw.received(); s != nil {
		fmt.Fprintf(stderr, "goronation run: %v を受けて、檻を止めた\n", s)
		return exitCodeForSignal(s)
	}
	return code
}

// runTarget は、doRun が檻で動かす作業ディレクトリと run dir (セッションか、ログイン用)。
type runTarget struct {
	id     string // セッション ID。--login では空
	work   string // /work に見せる
	runDir string // /run/goronation に見せる
}

// pickAgent は、この goronation run で動かすエージェントと、--session で再開する既存のセッション (--session でなければ nil) を決める。
//
// --session は、セッションを作ったエージェント (セッションの記録。記録の無い、エージェントを記録する前のセッションは legacySessionAgent) で
// 動かす。clone には、エージェントが置いた設定 (.claude/settings.json・opencode.json など) が残り、次にそこで動く
// エージェントが起動時に読んで実行する。別のエージェント (別の HOME の認証情報と、別の許可宛先を持つ) で使い回すと、
// 片方の檻が置いたものが、もう片方の檻で動く。--agent を省略したときは、記録のエージェントで動き、記録と違う --agent は断る。
func pickAgent(o runOptions, store *session.Store) (agentProfile, *session.Session, error) {
	if o.session == "" {
		return o.profile(), nil, nil
	}
	sess, err := store.Get(o.session)
	if err == nil {
		err = requireDir(sess.Clone)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return agentProfile{}, nil, fmt.Errorf("セッションを使えない: %s が無い。一覧: goronation sessions", o.session)
	}
	if err != nil {
		return agentProfile{}, nil, fmt.Errorf("セッションを使えない: %s。一覧: goronation sessions", strings.TrimPrefix(err.Error(), "session: "))
	}
	name, err := store.Agent(sess)
	if err != nil {
		return agentProfile{}, nil, fmt.Errorf("セッションを使えない: %s", strings.TrimPrefix(err.Error(), "session: "))
	}
	if name == "" {
		name = legacySessionAgent
	}
	recorded, ok := agentByName(name)
	if !ok {
		return agentProfile{}, nil, fmt.Errorf("このセッションのエージェント %q を、この goronation は知らない", name)
	}
	if o.agent != "" && o.agent != recorded.name {
		return agentProfile{}, nil, fmt.Errorf("このセッションは %s で作った。--agent %s では使えない。新しく作る: goronation run --agent %s --repo PATH",
			recorded.name, o.agent, o.agent)
	}
	return recorded, sess, nil
}

// resolveRepoTarget は、--repo (repo が空でなければ、新しいセッションを作る) か --session (existing を
// 再開する) の、run dir・clone・HOME を用意する (goronation run と goronation serve が共有する。--login はここを
// 通らない: doRun 自身が別に扱う)。作った・再開した旨の案内は、呼び手が (repo が空でなかったかを見て)
// 自分で出す (goronation run と goronation serve で文言が違うため、ここでは出さない)。
func resolveRepoTarget(ctx context.Context, store *session.Store, dirs agentDirs, agent agentProfile, repo, name, email string, existing *session.Session) (runTarget, string, error) {
	sess := existing
	if repo != "" {
		s, err := store.Create(ctx, session.CreateOptions{Repo: repo, Name: name, Email: email, Agent: agent.name})
		if err != nil {
			return runTarget{}, "", fmt.Errorf("セッションを作れない: %w", err)
		}
		sess = s
	}
	// HOME は repo ごと: セッションが記録した repo のキーの HOME (同じ repo のセッションは、同じ HOME。別の repo の HOME は、この檻に入らない)。
	key, err := store.HomeKey(sess)
	switch {
	case err != nil:
		return runTarget{}, "", fmt.Errorf("セッションを使えない: %s", strings.TrimPrefix(err.Error(), "session: "))
	case key == "":
		return runTarget{}, "", fmt.Errorf("このセッションは、HOME を repo ごとに分ける前に作った (使えない)。新しく作る: goronation run%s --repo PATH", agentFlagFor(agent))
	}
	home := dirs.homeFor(key)
	if err := ensureHome(agent, home, true); err != nil {
		return runTarget{}, "", fmt.Errorf("HOME を作れない: %w", err)
	}
	if err := os.MkdirAll(sess.Run, 0o700); err != nil {
		return runTarget{}, "", fmt.Errorf("run dir を作れない: %w", err)
	}
	return runTarget{id: sess.ID, work: sess.Clone, runDir: sess.Run}, home, nil
}

// prepareAgentLaunch は、--repo・--session・--login のどれでも共通する下ごしらえ: session.Store・
// エージェントの選択 (--session なら記録のエージェントで動かす)・エージェントの実行ファイルの解決・
// goronation 自身の実行ファイルの解決・エージェントの状態ディレクトリの用意、をまとめる (goronation run と
// goronation serve が共有する)。
func prepareAgentLaunch(stateDir, sessionID, agentFlag, bin string) (host bwrap.Host, store *session.Store, agent agentProfile, existing *session.Session, agentExe, self string, dirs agentDirs, err error) {
	host = bwrap.CurrentHost()
	store, err = session.NewStore(stateDir, host)
	if err != nil {
		return
	}
	// エージェントは、実行ファイルと HOME を決める前に知る (--session は、記録のエージェントで動かす)。
	agent, existing, err = pickAgent(runOptions{session: sessionID, agent: agentFlag}, store)
	if err != nil {
		return
	}
	agentExe, err = resolveAgentExe(agent, bin, os.Getenv(agent.exeEnv()), exec.LookPath)
	if err != nil {
		return
	}
	self, err = os.Executable()
	if err == nil {
		self, err = resolveExe(self)
	}
	if err != nil {
		err = fmt.Errorf("goronation 自身の実行ファイルを決められない: %w", err)
		return
	}
	dirs = agent.dirs(stateDir)
	// 認証情報の置き場は、エージェントごとに 1 つ (全 repo・--login の檻で共有する)。中身は、ホストは読まない。
	if err = ensureDir(dirs.auth); err != nil {
		err = fmt.Errorf("認証情報のディレクトリを作れない: %w", err)
		return
	}
	return
}

func doRun(ctx context.Context, o runOptions, sw *sigWatch, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goronation run: "+format+"\n", a...)
		return 1
	}
	stateDir, err := resolveStateDir(o.stateDir)
	if err != nil {
		return fail("%v", err)
	}
	// egress の UDS の path が長すぎるときは、何も作る前に断る (clone を作った後では、孤児のセッションが残る)。
	if err := checkSockPath(sockPathFor(stateDir, o), "egress"); err != nil {
		return fail("%v", err)
	}
	host, store, agent, existing, agentExe, self, dirs, err := prepareAgentLaunch(stateDir, o.session, o.agent, o.bin)
	if err != nil {
		return fail("%v", err)
	}

	var tgt runTarget
	var home string // repo ごとの HOME (--login では、ログイン用の HOME)
	switch {
	case o.login:
		tgt = runTarget{work: dirs.loginWork, runDir: dirs.loginRun}
		for _, d := range []string{tgt.work, tgt.runDir} {
			if err := ensureDir(d); err != nil {
				return fail("ログイン用のディレクトリを作れない: %v", err)
			}
		}
		home = dirs.loginHome
		if err := ensureHome(agent, home, false); err != nil { // 種は置かない: 初回の設定を、最後まで通す
			return fail("ログイン用の HOME を作れない: %v", err)
		}
	default:
		isNew := o.repo != ""
		tgt, home, err = resolveRepoTarget(ctx, store, dirs, agent, o.repo, o.name, o.email, existing)
		if err != nil {
			return fail("%v", err)
		}
		if isNew {
			fmt.Fprintf(stderr, "goronation run: セッション %s を作った\n", tgt.id)
		}
	}

	if o.login {
		fmt.Fprintln(stderr, "goronation run: "+agent.loginGuide())
	}

	// --push: git push・PR 作成の配線 (git.Policy・資格情報) を、檻を起こす前に用意する (作れなければ、檻を
	// 起動しない)。gateway.Handler 自体は startProxy が作る (監査を、egress の監査ログと同じ書き込み先に
	// 揃えるため。auditLog は startProxy の中でしか作らない)。
	var push *pushConfig
	if o.push != "" {
		repo, err := git.ParseRepo(o.push) // parseRunArgs が確かめ済みだが、ここでも確かめる (呼び手を信用しない)
		if err != nil {
			return fail("--push: %v", err)
		}
		// origin を、GitHub の URL に揃え直す (--push が有効な起動のたび、再開を含めて毎回。insteadOf の
		// 配線は cageEnv (pushEnv) が別途行う: ここでは、素の git fetch/pull/push origin ... が届く先の
		// remote を用意するだけで、新しい通信経路は増えない)。Create が直後に origin を外しているので
		// (session/create.go)、新規セッションでも、この呼び出しが無いと origin が無いまま動かない。
		if err := store.SetPushOrigin(ctx, tgt.work, repo.CloneURL()); err != nil {
			return fail("origin を設定できない: %v", err)
		}
		p, err := newPushConfig(stateDir, o.push, tgt.id)
		if err != nil {
			return fail("--push の配線を作れない: %v", err)
		}
		push = p
	}

	sum := runSummary{id: tgt.id, stateDir: stateDir, stateDirGiven: o.stateDir != "", authDir: dirs.auth, agent: agent}
	code := runCage(ctx, o, agent, sw, tgt, cageConfig{
		Host: host, Agent: agent, AgentExe: agentExe, GoroExe: self, CACerts: existingDir("/etc/ssl/certs"),
		RunDir: tgt.runDir, AgentHome: home, AuthDir: dirs.auth, Work: tgt.work, Term: os.Getenv("TERM"), TZ: hostTZ(), Args: cageArgs(o, agent),
		PushRepo: o.push, PushRefPrefix: push.refPrefix(), MCPServers: mcpServersFor(o.push),
	}, push, &sum, stderr)
	printRunSummary(stderr, sum)
	return code
}

// newPushConfig は、--push owner/repo (sessionID の名前空間限定) の pushConfig (startProxy が
// gateway.Handler を作るのに要るものだけ) を作る。stateDir は、goronation auth github が保存したトークンの場所
// (credfile.Store)。sessionID が git.NewPolicy の検査 (ref の要素として正しい 1〜64 文字) を通らなければ error
// (--login は、この呼び出し自体をしない。呼び手が保証する)。
func newPushConfig(stateDir, push, sessionID string) (*pushConfig, error) {
	repo, err := git.ParseRepo(push) // parseRunArgs が確かめ済みだが、ここでも確かめる (呼び手を信用しない)
	if err != nil {
		return nil, err
	}
	policy, err := git.NewPolicy(repo, sessionID)
	if err != nil {
		return nil, fmt.Errorf("セッション ID: %w", err)
	}
	creds, err := credfile.New(stateDir)
	if err != nil {
		return nil, err
	}
	return &pushConfig{Policy: policy, Credentials: creds}, nil
}

// sampleSessionID は、セッション ID と同じ長さの例 (session の ID の形。セッションを作る前に、UDS の path の長さを調べるため)。
const sampleSessionID = "00000000-000000-000000"

// sockPathFor は、o の goronation run が使う egress の UDS の path。セッションの ID は、同じ長さの例で代える。
func sockPathFor(stateDir string, o runOptions) string {
	if o.login {
		return filepath.Join(o.profile().dirs(stateDir).loginRun, proxySockName)
	}
	return filepath.Join(stateDir, "sessions", sampleSessionID, "run", proxySockName)
}

// cageArgs は、エージェントへの引数: --login なら、エージェントの loginArgs の後ろに、利用者の引数を付ける。
func cageArgs(o runOptions, p agentProfile) []string {
	if o.login {
		return append(slices.Clone(p.loginArgs), o.agentArgs...)
	}
	return o.agentArgs
}

// runCage は、egress を起こして、檻を起動し、終わるのを待つ。終了コードを返す。
//
// 標準入出力の 3 つ全てが実端末なら (isTTYFile)、goronation run 自身が pty を用意し (cfg.PTY)、檻には、その slave を
// 渡す (ホストの実端末には直結しない)。goronation run は、ホストの実端末を raw モードにして、pty の master との間を
// そのまま中継する (中身は解釈・模倣しない)。3 つのどれかが端末でなければ (redirect・pipe。テストの多くもここ)、
// これまでどおり、ホストの標準入出力をそのまま渡す。
func runCage(ctx context.Context, o runOptions, agent agentProfile, sw *sigWatch, tgt runTarget, cfg cageConfig, push *pushConfig, sum *runSummary, stderr io.Writer) int {
	proxy, err := startProxy(tgt.runDir, allowList(agent, o.allow), push)
	if err != nil {
		fmt.Fprintf(stderr, "goronation run: egress を起動できない: %v\n", err)
		return 1
	}
	sum.logPath = proxy.logPath
	defer func() {
		sum.dropped, sum.serveErr = proxy.Close()
		if top, more, err := deniedTargets(proxy.logPath, proxy.logStart, maxDeniedShown); err == nil {
			sum.denied, sum.deniedMore = top, more
		}
	}()

	cfg.PTY = isTTYFile(os.Stdin) && isTTYFile(os.Stdout) && isTTYFile(os.Stderr)
	spec := cageSpec(cfg)

	// 檻の中のプロセスは、端末の設定を変えられる (cfg.PTY のときは、goronation run 自身が raw モードにする)。標準入力が
	// 端末なら、起動の前に保存し、どの経路で終わっても (正常・シグナルでの取り消し・エラー)、終了後の案内を出す
	// 前に戻す。os.Stdin.Fd() は使わない: 一度呼ぶと、その *os.File は恒久的に blocking 扱いになり、pty 中継の
	// SetReadDeadline (中継を止めるときに使う) が効かなくなる (SyscallConn 経由なら、fd は non-blocking のまま)。
	var term *termState
	if err := ctlFile(os.Stdin, func(fd int) (err error) { term, err = saveTermios(fd); return }); err != nil {
		fmt.Fprintf(stderr, "goronation run: 端末の設定を保存できない (終了後に戻せない): %v\n", err)
	}
	defer restoreTermios(term, stderr)

	if cfg.PTY {
		master, slave, err := openHostPty()
		if err != nil {
			fmt.Fprintf(stderr, "goronation run: pty を用意できない: %v\n", err)
			return 1
		}
		defer slave.Close() // 檻が fork/exec で引き継いだ後は、ホスト側の複製は要らない
		spec.Stdin, spec.Stdout, spec.Stderr = slave, slave, slave
		if err := term.setRaw(); err != nil {
			fmt.Fprintf(stderr, "goronation run: 実端末を raw モードにできない: %v\n", err)
		}
		relay := startPtyRelay(os.Stdin, os.Stdout, master)
		defer relay.stop()
	} else {
		spec.Stdin, spec.Stdout, spec.Stderr = os.Stdin, os.Stdout, os.Stderr
	}

	if ctx.Err() != nil { // 起動の前に、シグナルを受けていた
		return 1
	}
	sw.enterCage()
	c, err := bwrap.Start(ctx, spec)
	if err != nil {
		fmt.Fprintf(stderr, "goronation run: 檻を起動できない: %v\n", err)
		return 1
	}
	sum.started = true
	return exitCodeOf(c.Wait(), stderr)
}

// exitCodeOf は、檻の Wait の結果を、終了コードにする (シグナルで死んだら 128+番号)。
func exitCodeOf(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		fmt.Fprintf(stderr, "goronation run: 檻を待てない: %v\n", err)
		return 1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

// resolveStateDir は、状態を置く絶対・クリーンな path: --state-dir、なければ既定 (session.DefaultStateDir)。
func resolveStateDir(flagVal string) (string, error) {
	if flagVal == "" {
		return session.DefaultStateDir()
	}
	return filepath.Abs(flagVal)
}

// requireDir は、p が本物のディレクトリ (symlink でない) であることを確かめる。
func requireDir(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s がディレクトリではない", p)
	}
	return nil
}

// existingDir は、p が (symlink を辿って) ディレクトリなら p、そうでなければ空。
func existingDir(p string) string {
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	return ""
}

// maxDeniedShown は、終了後に表示する、拒否された宛先の数の上限。
const maxDeniedShown = 10

// runSummary は、goronation run の終了後に表示する内容。
type runSummary struct {
	id            string // セッション ID。--login では空
	stateDir      string
	stateDirGiven bool         // --state-dir を指定したか (再開のコマンドに含める)
	authDir       string       // 認証情報の置き場 (--login の終了後に案内する)
	agent         agentProfile // 空 (ゼロ値) は既定のエージェントとして扱う
	started       bool         // 檻を起動できたか
	logPath       string       // egress の監査ログ (起動できなかったときは空)
	denied        []deniedTarget
	deniedMore    int
	dropped       int64 // 捨てた監査の行
	serveErr      error // egress の待ち受けの異常な終了
}

// printRunSummary は、goronation run の終了後の案内を w に出す。ユーザーが次にすること (コマンド) と、拒否された宛先だけを、短く出す:
// セッション ID・再開と取り出しのコマンド (--login なら、次のコマンド)・拒否された宛先 (許可するコマンドつき)。
// 理由・経緯の説明は、出さない (goronation run -h に書く)。檻を起動できなかったときは、何も出さない。
func printRunSummary(w io.Writer, s runSummary) {
	if !s.started {
		return // 檻を起動できなかった: 原因は、すでに表示した。必ず失敗する再開や、中身の無い取り出しを案内しない
	}
	stateFlag := ""
	if s.stateDirGiven {
		stateFlag = " --state-dir " + shellQuote(s.stateDir)
	}
	agentFlag := ""
	if s.agent.name != "" {
		agentFlag = agentFlagFor(s.agent)
	}
	fmt.Fprintln(w)
	if s.id == "" {
		fmt.Fprintf(w, "ログイン状態: %s\n", sanitize(s.authDir))
		fmt.Fprintf(w, "次は: goronation run%s%s --repo PATH\n", agentFlag, stateFlag)
	} else {
		fmt.Fprintf(w, "セッション: %s\n", s.id)
		fmt.Fprintf(w, "  再開:   goronation run%s%s --session %s\n", agentFlag, stateFlag, s.id)
		fmt.Fprintf(w, "  取り出し: goronation export%s %s\n", stateFlag, s.id)
	}
	if len(s.denied) > 0 {
		fmt.Fprintln(w, "拒否された宛先:")
		example := "" // --allow の例には、説明のある宛先 (許可不要・先にすることがあるもの) を使わない
		for _, d := range s.denied {
			line := fmt.Sprintf("  %s (%d 回)", sanitize(d.Target), d.Count)
			if note, ok := s.agent.denyNotes[d.Target]; ok {
				line += " — " + note
			} else if example == "" {
				example = d.Target
			}
			fmt.Fprintln(w, line)
		}
		if s.deniedMore > 0 {
			fmt.Fprintf(w, "  ほか %d 件\n", s.deniedMore)
		}
		if example != "" || s.deniedMore > 0 {
			if example == "" {
				example = "HOST:PORT"
			}
			fmt.Fprintf(w, "許可するには、--allow %s を付けて起動する\n", sanitize(example))
		}
		if s.logPath != "" {
			fmt.Fprintf(w, "監査ログ: %s\n", sanitize(s.logPath))
		}
	}
	if s.dropped > 0 {
		fmt.Fprintf(w, "監査の %d 行を、書けずに捨てた (ディスクを確認する)\n", s.dropped)
	}
	if s.serveErr != nil {
		fmt.Fprintf(w, "egress が異常終了した: %v\n", s.serveErr)
	}
}

// agentFlagFor は、案内のコマンドに足す " --agent NAME" (既定のエージェントなら、要らないので空)。
func agentFlagFor(p agentProfile) string {
	if isDefaultAgent(p.name) {
		return ""
	}
	return " --agent " + p.name
}

// shellQuote は、s を、シェルの 1 語 (単一引用符で囲んだもの) にする。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

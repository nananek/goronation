//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nananek/goronation/cmd/internal/session"
	"github.com/nananek/goronation/sandbox/bwrap"
)

const runUsage = `使い方: goro run (--repo PATH | --session ID | --login) [オプション] [-- claudeへの引数...]

claude を、檻 (ネットワークの無い bwrap) の中で、ホストの repo の private clone の上で動かす。
ホストの作業ツリー・.git・~/.claude・~/.ssh・環境変数は、檻から見えない。clone されるのはコミット済みの内容だけ。

  --repo PATH       PATH (ローカルの repo) の private clone を作り、その中で claude を起動する
  --session ID      前の goro run のセッションを再開する (同じ clone が見える)
  --login           repo・clone 無しで、空の作業ディレクトリで claude auth login を起動する (ログイン状態は、檻専用の HOME に残る)
  --name N          clone の user.name (--repo のとき。既定は goro)
  --email E         clone の user.email (--repo のとき)
  --state-dir DIR   状態 (セッション・檻専用の HOME) を置く場所 (既定は $XDG_STATE_HOME/goro か ~/.local/state/goro)
  --claude PATH     claude の実行ファイル (既定は環境変数 GORO_CLAUDE か、PATH の claude)
  --allow HOST:PORT 檻から届く宛先を足す (何度でも書ける。既定は api.anthropic.com:443 と platform.claude.com:443)
  -- ARGS...        claude に渡す引数 (--login のときは、claude auth login の後ろに付く)

例:
  goro run --login                    初回。出た URL をホストのブラウザで開き、出たコードを貼る
  goro run --repo ~/work/foo          foo の private clone の中で claude と対話する
  goro run --session ID -- --resume   再開する (-- の後ろは claude への引数)
  goro export ID                      成果 (コミット) を bundle にして、取り込みのコマンドを表示する

起動後の Ctrl-C は、claude の中断として効く (goro run 自身は終了しない)。止めるときは、claude の終了操作か、別の端末から goro run に SIGTERM。
終わると、セッション ID・再開と取り出しのコマンド・拒否された宛先を表示する。
`

// runOptions は、goro run の引数。
type runOptions struct {
	repo, session string
	login         bool
	name, email   string
	stateDir      string // 空なら既定
	claude        string // 空なら GORO_CLAUDE か PATH
	allow         []string
	claudeArgs    []string
}

// stringList は、何度でも書ける文字列のオプション。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// parseRunArgs は goro run の引数を解釈する。不正なら、理由と使い方を stderr に出して error を返す。
// -h のときは、使い方を出して flag.ErrHelp を返す。
func parseRunArgs(args []string, stderr io.Writer) (runOptions, error) {
	var o runOptions
	head, tail := args, []string(nil)
	if i := indexOf(args, "--"); i >= 0 {
		head, tail = args[:i], args[i+1:]
	}
	flags := flag.NewFlagSet("goro run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, runUsage) }
	var allow stringList
	flags.StringVar(&o.repo, "repo", "", "")
	flags.StringVar(&o.session, "session", "", "")
	flags.BoolVar(&o.login, "login", false, "")
	flags.StringVar(&o.name, "name", "", "")
	flags.StringVar(&o.email, "email", "", "")
	flags.StringVar(&o.stateDir, "state-dir", "", "")
	flags.StringVar(&o.claude, "claude", "", "")
	flags.Var(&allow, "allow", "")
	if err := flags.Parse(head); err != nil {
		return o, err // flag が、理由と使い方を出している
	}
	o.allow, o.claudeArgs = allow, tail

	fail := func(format string, a ...any) (runOptions, error) {
		fmt.Fprintf(stderr, "goro run: "+format+"\n", a...)
		fmt.Fprint(stderr, runUsage)
		return o, errors.New("引数が不正")
	}
	if flags.NArg() > 0 {
		return fail("余計な引数 %q (claude への引数は -- の後ろに書く)", flags.Arg(0))
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
		return fail("--name と --email は、--repo のときだけ使える (clone の名義)")
	}
	if err := checkAllow(o.allow); err != nil {
		return fail("%v", err)
	}
	return o, nil
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

// resolveClaude は、檻に見せる claude の実体を決める: --claude、なければ環境変数 GORO_CLAUDE (テスト用に、同じ効果)、
// なければ PATH の claude。どれも、symlink を辿った実体にする (native 版は、~/.local/bin/claude が、版ごとの実体への symlink)。
func resolveClaude(flagVal, envVal string, lookPath func(string) (string, error)) (string, error) {
	p := flagVal
	if p == "" {
		p = envVal
	}
	if p == "" {
		found, err := lookPath("claude")
		if err != nil {
			return "", fmt.Errorf("claude が見つからない (PATH に置くか、--claude か GORO_CLAUDE で指す): %w", err)
		}
		p = found
	}
	real, err := resolveExe(p)
	if err != nil {
		return "", fmt.Errorf("claude (%s) を使えない: %w", p, err)
	}
	return real, nil
}

// ensureDir は、自分が使う dir を 0700 で作る (あれば、権限を 0700 にする)。
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// runRun は goro run の本体で、終了コードを返す (claude の終了コード。goro run 自身の失敗は 1、引数の不正は 2)。
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
		fmt.Fprintf(stderr, "goro run: %v を受けて、檻を止めた\n", s)
		return exitCodeForSignal(s)
	}
	return code
}

// runTarget は、doRun が檻で動かす作業ディレクトリと run dir (セッションか、ログイン用)。
type runTarget struct {
	id     string // セッション ID。--login では空
	work   string // /work に見せる
	runDir string // /run/goro に見せる
}

func doRun(ctx context.Context, o runOptions, sw *sigWatch, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro run: "+format+"\n", a...)
		return 1
	}
	stateDir, err := resolveStateDir(o.stateDir)
	if err != nil {
		return fail("%v", err)
	}
	// egress の UDS の path が長すぎるときは、何も作る前に断る (clone を作った後では、孤児のセッションが残る)。
	if err := checkSockPath(sockPathFor(stateDir, o)); err != nil {
		return fail("%v", err)
	}
	claudeExe, err := resolveClaude(o.claude, os.Getenv("GORO_CLAUDE"), exec.LookPath)
	if err != nil {
		return fail("%v", err)
	}
	self, err := os.Executable()
	if err == nil {
		self, err = resolveExe(self)
	}
	if err != nil {
		return fail("goro 自身の実行ファイルを決められない: %v", err)
	}
	host := bwrap.CurrentHost()
	store, err := session.NewStore(stateDir, host)
	if err != nil {
		return fail("%v", err)
	}
	agentHome := filepath.Join(stateDir, "home")
	if err := ensureDir(agentHome); err != nil {
		return fail("エージェントの HOME を作れない: %v", err)
	}

	var tgt runTarget
	switch {
	case o.login:
		tgt = runTarget{work: filepath.Join(stateDir, "login-work"), runDir: filepath.Join(stateDir, "login-run")}
		for _, d := range []string{tgt.work, tgt.runDir} {
			if err := ensureDir(d); err != nil {
				return fail("ログイン用のディレクトリを作れない: %v", err)
			}
		}
	default:
		var sess *session.Session
		if o.repo != "" {
			sess, err = store.Create(ctx, session.CreateOptions{Repo: o.repo, Name: o.name, Email: o.email})
			if err != nil {
				return fail("セッションを作れない: %v", err)
			}
			fmt.Fprintf(stderr, "goro run: セッション %s を作った\n", sess.ID)
		} else {
			sess, err = store.Get(o.session)
			if err == nil {
				err = requireDir(sess.Clone)
			}
			if err != nil {
				return fail("セッションを使えない: %v", err)
			}
		}
		if err := os.MkdirAll(sess.Run, 0o700); err != nil {
			return fail("run dir を作れない: %v", err)
		}
		tgt = runTarget{id: sess.ID, work: sess.Clone, runDir: sess.Run}
	}

	sum := runSummary{id: tgt.id, stateDir: stateDir, stateDirGiven: o.stateDir != "", agentHome: agentHome}
	code := runCage(ctx, o, sw, tgt, cageConfig{
		Host: host, ClaudeExe: claudeExe, GoroExe: self, CACerts: existingDir("/etc/ssl/certs"),
		RunDir: tgt.runDir, AgentHome: agentHome, Work: tgt.work, Term: os.Getenv("TERM"), Args: cageArgs(o),
	}, &sum, stderr)
	printRunSummary(stderr, sum)
	return code
}

// sampleSessionID は、セッション ID と同じ長さの例 (session の ID の形。セッションを作る前に、UDS の path の長さを調べるため)。
const sampleSessionID = "00000000-000000-000000"

// sockPathFor は、o の goro run が使う egress の UDS の path。セッションの ID は、同じ長さの例で代える。
func sockPathFor(stateDir string, o runOptions) string {
	if o.login {
		return filepath.Join(stateDir, "login-run", proxySockName)
	}
	return filepath.Join(stateDir, "sessions", sampleSessionID, "run", proxySockName)
}

// cageArgs は、claude への引数: --login なら auth login の後ろに、利用者の引数を付ける。
func cageArgs(o runOptions) []string {
	if o.login {
		return append([]string{"auth", "login"}, o.claudeArgs...)
	}
	return o.claudeArgs
}

// runCage は、egress を起こして、檻を起動し、終わるのを待つ。終了コードを返す。
func runCage(ctx context.Context, o runOptions, sw *sigWatch, tgt runTarget, cfg cageConfig, sum *runSummary, stderr io.Writer) int {
	proxy, err := startProxy(tgt.runDir, allowList(o.allow))
	if err != nil {
		fmt.Fprintf(stderr, "goro run: egress を起動できない: %v\n", err)
		return 1
	}
	sum.logPath = proxy.logPath
	defer func() {
		sum.dropped, sum.serveErr = proxy.Close()
		if top, more, err := deniedTargets(proxy.logPath, proxy.logStart, maxDeniedShown); err == nil {
			sum.denied, sum.deniedMore = top, more
		}
	}()

	spec := cageSpec(cfg)
	spec.Stdin, spec.Stdout, spec.Stderr = os.Stdin, os.Stdout, os.Stderr
	if ctx.Err() != nil { // 起動の前に、シグナルを受けていた
		return 1
	}
	// 檻の中のプロセスは、端末の設定を変えられる。標準入力が端末なら、起動の前に保存し、どの経路で終わっても (正常・
	// シグナルでの取り消し・エラー)、終了後の案内を出す前に戻す。
	term, err := saveTermios(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(stderr, "goro run: 端末の設定を保存できない (終了後に戻せない): %v\n", err)
	}
	defer restoreTermios(term, stderr)
	sw.enterCage()
	c, err := bwrap.Start(ctx, spec)
	if err != nil {
		fmt.Fprintf(stderr, "goro run: 檻を起動できない: %v\n", err)
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
		fmt.Fprintf(stderr, "goro run: 檻を待てない: %v\n", err)
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

// runSummary は、goro run の終了後に表示する内容。
type runSummary struct {
	id            string // セッション ID。--login では空
	stateDir      string
	stateDirGiven bool // --state-dir を指定したか (再開のコマンドに含める)
	agentHome     string
	started       bool   // 檻を起動できたか
	logPath       string // egress の監査ログ (起動できなかったときは空)
	denied        []deniedTarget
	deniedMore    int
	dropped       int64 // 捨てた監査の行
	serveErr      error // egress の待ち受けの異常な終了
}

// printRunSummary は、goro run の終了後の案内を w に出す: セッション ID・再開と取り出しのコマンド・拒否された宛先。
// 檻を起動できなかったときは、何も出さない。
func printRunSummary(w io.Writer, s runSummary) {
	if !s.started {
		return // 檻を起動できなかった: 原因は、すでに表示した。必ず失敗する再開や、中身の無い取り出しを案内しない
	}
	stateFlag := ""
	if s.stateDirGiven {
		stateFlag = " --state-dir " + shellQuote(s.stateDir)
	}
	fmt.Fprintln(w)
	if s.id == "" {
		fmt.Fprintf(w, "檻専用の HOME (ログイン状態が残る): %s\n", sanitize(s.agentHome))
		fmt.Fprintf(w, "  次は: goro run%s --repo PATH\n", stateFlag)
	} else {
		fmt.Fprintf(w, "セッション: %s\n", s.id)
		fmt.Fprintf(w, "  再開:         goro run%s --session %s\n", stateFlag, s.id)
		fmt.Fprintf(w, "  成果の取り出し: goro export%s %s\n", stateFlag, s.id)
	}
	if s.logPath != "" {
		fmt.Fprintf(w, "egress の監査ログ: %s\n", sanitize(s.logPath))
	}
	if len(s.denied) > 0 {
		fmt.Fprintln(w, "許可の一覧に無く、拒否された宛先 (最大 10 件):")
		for _, d := range s.denied {
			fmt.Fprintf(w, "  %s (%d 回)\n", sanitize(d.Target), d.Count)
		}
		if s.deniedMore > 0 {
			fmt.Fprintf(w, "  ほか %d 件 (監査ログを見る)\n", s.deniedMore)
		}
		fmt.Fprintf(w, "  必要な宛先は、--allow HOST:PORT を付けて、もう一度起動する (例: --allow %s)\n", sanitize(s.denied[0].Target))
	}
	if s.dropped > 0 {
		fmt.Fprintf(w, "注意: 監査の行を %d 行、書けずに捨てた (監査ログが欠けている)\n", s.dropped)
	}
	if s.serveErr != nil {
		fmt.Fprintf(w, "注意: egress の待ち受けが異常に終わった: %v\n", s.serveErr)
	}
}

// shellQuote は、s を、シェルの 1 語 (単一引用符で囲んだもの) にする。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

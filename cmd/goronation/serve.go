//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// serveUsage は、goronation serve -h の使い方。
const serveUsage = `使い方: goronation serve (--repo PATH [--name N] [--email E]) | --session ID
                   [--chat] [--agent NAME] [--state-dir DIR] [--socket PATH] [-- ARGS...]

goronation serve は、認証済みの相手 (通常は goronation web) から UDS 経由でだけ繋がる、端末ビュー (pty の生
バイト中継) の WebSocket サーバーを起こす。UDS に繋げること自体を信頼の境界にする (goronation serve 自身は
cookie も WebAuthn も持たない。認証は goronation web が済ませている前提)。goronation run --repo/--session と同じ
組み立てで、檻を 1 つ起こす (プロセスの寿命いっぱい生かす)。

  --repo PATH     PATH (ローカルの repo) の private clone を作り、その中でエージェントを起動する
  --session ID    前に goronation serve 自身が作ったセッションを再開する (--repo とは同時に使えない)
  --chat          端末ビューの代わりに、構造化チャット (stream-json を檻で回す。SSE の GET /events と、POST /message・
                  /permission・/stop) を、UDS chat.sock で出す。エージェントは claude だけ。root では動かせない (非 root の利用者で動かす)。
                  最初の指示は、チャット (POST /message) から送る (起動しただけでは、エージェントに何も送らない)
  --agent NAME    動かすエージェント (goronation run と同じ表。--session のときは、記録したものと違うと断る)
  --name N        clone の user.name (--repo のとき)
  --email E       clone の user.email (--repo のとき)
  --state-dir DIR 状態を置く場所 (既定は $XDG_STATE_HOME/goronation か ~/.local/state/goronation)
  --socket PATH   UDS の path (既定は <state-dir>/groups/default/sessions/<session-id>/term.sock (--chat のときは chat.sock)。
                  goronation web は、同じ既定の計算式で dial するので、通常は指定しなくてよい)
  -- ARGS...      エージェントへの引数 (実運用では通常は要らない: エージェントの既定の対話モードで
                  起動し、操作は goronation web 越しの端末ビューで行う)

見送った範囲 (この版には無い): --push・--allow・--bin。複数セッションの並行管理。エージェントが
終了したら、サーバー自体は落とさず、WebSocket が繋がらなくなるだけ (新しいセッションは、goronation serve を
再起動して作る)。
`

// serveShutdownTimeout は、SIGTERM 等を受けてからの graceful shutdown (http.Server.Shutdown) に許す猶予。
const serveShutdownTimeout = 5 * time.Second

// runServe は goronation serve の本体。
func runServe(args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goronation serve: "+format+"\n", a...)
		return exitUsage
	}
	head, tail := args, []string(nil)
	if i := indexOf(args, "--"); i >= 0 {
		head, tail = args[:i], args[i+1:]
	}
	flags := flag.NewFlagSet("goronation serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, serveUsage) }
	stateDirFlag := flags.String("state-dir", "", "")
	repoFlag := flags.String("repo", "", "")
	sessionFlag := flags.String("session", "", "")
	agentFlag := flags.String("agent", "", "")
	nameFlag := flags.String("name", "", "")
	emailFlag := flags.String("email", "", "")
	socketFlag := flags.String("socket", "", "")
	chatFlag := flags.Bool("chat", false, "")
	if err := flags.Parse(head); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		return fail("余計な引数 %q", flags.Arg(0))
	}
	if *repoFlag == "" && *sessionFlag == "" {
		return fail("--repo か --session のどちらかが要る")
	}
	if *repoFlag != "" && *sessionFlag != "" {
		return fail("--repo と --session は同時に指定できない")
	}
	if (*nameFlag != "" || *emailFlag != "") && *repoFlag == "" {
		return fail("--name と --email は、--repo のときだけ使える")
	}
	if *agentFlag != "" {
		if _, ok := agentByName(*agentFlag); !ok {
			return fail("--agent は %s: %q", agentNames(), *agentFlag)
		}
	}
	if *chatFlag && os.Geteuid() == 0 { // 何かを作る前に、分かりやすく断る (L3)
		return fail("%v", errChatRoot)
	}
	stateDir, err := resolveStateDir(*stateDirFlag)
	if err != nil {
		return fail("%v", err)
	}

	// UDS の path が長いと、後で失敗する (セッションと檻を作った後になる: L-D)。何も作る前に、確かめる (ID は同じ長さの仮のものでよい)。
	earlyWhat, earlySock := "端末ビュー", termSocketPath
	if *chatFlag {
		earlyWhat, earlySock = "チャット", chatSocketPath
	}
	earlyPath := *socketFlag
	if earlyPath == "" {
		earlyPath = earlySock(stateDir, defaultGroup, chatSockPlaceholderID)
	}
	if err := checkSockPath(earlyPath, earlyWhat); err != nil {
		return fail("%v", err)
	}

	// goronation serve は、goronation run のような、ホストの端末を檻と共有する前提の signal 処理 (sigWatch) を
	// 使わない (端末ビューは WebSocket 越しで、檻とホストの制御端末は無関係)。SIGINT・SIGTERM・SIGHUP
	// は、単純に ctx を取り消すだけにする (serve_signals.go)。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSig := watchServeSignals(cancel)
	defer stopSig()

	// 端末ビュー (term) か、構造化チャット (--chat) のどちらか 1 つの檻を起こす。以降は、id・待ち・UDS の HTTP ハンドラだけが違う。
	var (
		id      string
		wait    func()
		handler http.Handler
		what    = "端末ビュー"
		sockOf  = termSocketPath
	)
	if *chatFlag {
		chatS, err := startServeChatSession(ctx, stateDir, *sessionFlag, *agentFlag, *nameFlag, *emailFlag, *repoFlag, tail, stderr)
		if err != nil {
			return fail("チャットのセッションを起動できない: %v", err)
		}
		id, wait, handler, what, sockOf = chatS.id, func() { chatS.Wait() }, newChatHandler(chatS), "チャット", chatSocketPath
	} else {
		term, err := startServeTermSession(ctx, stateDir, *sessionFlag, *agentFlag, *nameFlag, *emailFlag, *repoFlag, tail, stderr)
		if err != nil {
			return fail("端末ビューのセッションを起動できない: %v", err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { handleTerminal(term, w, r) })
		mux.HandleFunc("GET /version", handleVersion)
		id, wait, handler = term.id, func() { term.Wait() }, mux
	}
	// 戻る前に、檻の後片付け (proxy.Close・master.Close) が終わるまで待つ。ただし、待つだけでは
	// ハングする: 檻は ctx が取り消されたときにしか終わらないが、defer は登録順と逆に走るので、
	// このまま defer wait() とだけ書くと、外側の defer cancel() より先に (cancel が走る前に)
	// 実行され、wait が無期限にブロックする (攻撃者視点レビューで発見。net.Listen の失敗など、
	// シグナルを経由しない異常系の return で起きる)。cancel を、ここで明示的に先に呼ぶ (cancel は
	// 冪等なので、外側の defer cancel() と重複しても安全)。
	defer func() { cancel(); wait() }()

	sockPath := *socketFlag
	if sockPath == "" {
		sockPath = sockOf(stateDir, defaultGroup, id)
	}
	if err := checkSockPath(sockPath, what); err != nil {
		return fail("%v", err)
	}
	sockDir := filepath.Dir(sockPath)
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return fail("UDS の置き場所を作れない: %v", err)
	}
	// 同じソケットを、同時に 2 つの goronation serve が使うことは、ロックで断る (前の起動が残した UDS は、
	// ロックを持てたときだけ消す。proxy.go の startProxy と同じパターン)。
	lock, err := lockDir(sockDir, "同じセッションの"+what+"を、別の goronation serve がすでに待ち受けている")
	if err != nil {
		return fail("%v", err)
	}
	defer lock.Close()
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("前の UDS を消せない: %v", err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return fail("UDS で待ち受けられない: %v", err)
	}
	defer l.Close()
	defer os.Remove(sockPath)
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return fail("UDS の権限を設定できない: %v", err)
	}

	srv := &http.Server{Handler: handler}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	// goronation web は、この行 (path と session ID の両方を含む) を見て、UDS が使えるようになったことと、
	// 対応するセッション ID を知る (新しく作ったセッション (--repo) の ID は、起動前には分からないため)。
	fmt.Fprintf(stderr, "goronation serve: %s で待ち受けている (session=%s)\n", sockPath, id)
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "goronation serve: 終了: %v\n", err)
		return 1
	}
	return 0
}

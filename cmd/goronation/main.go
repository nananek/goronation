package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `使い方: goronation <サブコマンド> [引数...]

サブコマンド:
  run       エージェント (claude・opencode) を、檻の中の private clone の上で動かす (goronation run -h)
  export    セッションの成果を bundle にして取り出す (goronation export -h)
  sessions  セッションの一覧を表示する (goronation sessions -h)
  auth      資格情報 (github のトークン) を、ホストのファイルに保存する (goronation auth -h)
  pr        PR を作る (檻の中。goronation run --push が要る) か、ready for review にする (ホスト) (goronation pr -h)
  mcp       MCP (Model Context Protocol) のサーバーとして動く (檻の中。goronation run --push が、エージェントに自動で登録する)
  serve     UDS 越しにだけ繋がる、端末ビューの WebSocket サーバーを起こす (goronation serve -h。goronation web が使う)
  web       WebAuthn でログインしたブラウザからセッションを選ぶ/始める、常駐の HTTP サーバーを起こす (goronation web -h)
  init      檻の中でリレーを起こし、子プロセスを起動する (goronation init -h。goronation run が使う)
`

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch は、最初の引数のサブコマンドへ振り分け、終了コードを返す。
func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runRun(args[1:], stderr)
	case "export":
		return runExport(args[1:], stdout, stderr)
	case "sessions":
		return runSessions(args[1:], stdout, stderr)
	case "auth":
		return runAuth(args[1:], os.Stdin, stdout, stderr)
	case "pr":
		return runPr(args[1:], stdout, stderr)
	case "mcp":
		return runMCP(args[1:], os.Stdin, stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "web":
		return runWeb(args[1:], stdout, stderr)
	case "init":
		return runInit(args[1:], stderr)
	case "__exec-hardened":
		return runExecHardened(args[1:], stderr)
	case "-h", "--help":
		fmt.Fprint(stderr, usage)
		return 0
	}
	fmt.Fprintf(stderr, "goronation: 未知のサブコマンド: %q\n%s", args[0], usage)
	return exitUsage
}

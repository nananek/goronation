package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `使い方: goro <サブコマンド> [引数...]

サブコマンド:
  run       エージェント (claude・opencode) を、檻の中の private clone の上で動かす (goro run -h)
  export    セッションの成果を bundle にして取り出す (goro export -h)
  sessions  セッションの一覧を表示する (goro sessions -h)
  init      檻の中でリレーを起こし、子プロセスを起動する (goro init -h。goro run が使う)
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
	case "init":
		return runInit(args[1:], stderr)
	case "-h", "--help":
		fmt.Fprint(stderr, usage)
		return 0
	}
	fmt.Fprintf(stderr, "goro: 未知のサブコマンド: %q\n%s", args[0], usage)
	return exitUsage
}

package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `使い方: goro <サブコマンド> [引数...]

サブコマンド:
  init    檻の中でリレーを起こし、子プロセスを起動する (goro init -h)
`

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stderr))
}

// dispatch は、最初の引数のサブコマンドへ振り分け、終了コードを返す。
func dispatch(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stderr)
	case "-h", "--help":
		fmt.Fprint(stderr, usage)
		return 0
	}
	fmt.Fprintf(stderr, "goro: 未知のサブコマンド: %q\n%s", args[0], usage)
	return exitUsage
}

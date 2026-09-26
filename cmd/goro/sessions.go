//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/nananek/goronation/cmd/internal/session"
	"github.com/nananek/goronation/sandbox/bwrap"
)

const sessionsUsage = `使い方: goro sessions [--state-dir DIR]

セッション (goro run --repo が作った private clone) の一覧を、古い順に表示する: ID・作成日時・エージェント・元の repo 名。
エージェントは、セッションを作ったもの (記録の無い古いセッションは claude。記録を読めないものは ?)。goro run --session は、そのエージェントで動かす。

  --state-dir DIR   状態を置く場所 (goro run と同じ。既定は $XDG_STATE_HOME/goro か ~/.local/state/goro)
`

// runSessions は goro sessions の本体で、終了コードを返す。
func runSessions(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("goro sessions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, sessionsUsage) }
	stateDir := flags.String("state-dir", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "goro sessions: 余計な引数 %q\n%s", flags.Arg(0), sessionsUsage)
		return exitUsage
	}
	dir, err := resolveStateDir(*stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "goro sessions: %v\n", err)
		return 1
	}
	store, err := session.NewStore(dir, bwrap.CurrentHost())
	if err != nil {
		fmt.Fprintf(stderr, "goro sessions: %v\n", err)
		return 1
	}
	list, err := store.List()
	if err != nil {
		fmt.Fprintf(stderr, "goro sessions: %v\n", err)
		return 1
	}
	printSessions(stdout, list)
	return 0
}

// printSessions は、セッションの一覧を w に出す (ID・作成日時 (ローカル時刻)・エージェント・元の repo 名)。
func printSessions(w io.Writer, list []session.Info) {
	if len(list) == 0 {
		fmt.Fprintln(w, "セッションは無い")
		return
	}
	for _, in := range list {
		repo := in.Repo
		if repo == "" {
			repo = "-"
		}
		agent := in.Agent
		if agent == "" {
			agent = claudeProfile.name // エージェントを記録する前に作ったセッションは、claude で作られた
		}
		fmt.Fprintf(w, "%s  %s  %s  %s\n", in.ID, in.Created.Local().Format("2006-01-02 15:04:05"), agent, repo)
	}
}

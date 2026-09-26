//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/nananek/goronation/cmd/internal/session"
	"github.com/nananek/goronation/sandbox/bwrap"
)

const exportUsage = `使い方: goro export [--state-dir DIR] SESSION-ID

セッションの clone のコミットを、bundle (1 ファイル) にして取り出し、その path・ブランチ・取り込みのコマンドを表示する。
bundle は、使い捨ての檻の中で作る。ホストは、clone の中で git を実行せず、bundle も取り込まない:
表示されたコマンドは、自分の repo で、自分で実行する (transfer.fsckObjects=true で、object を検査する)。

  --state-dir DIR   状態を置く場所 (goro run と同じ。既定は $XDG_STATE_HOME/goro か ~/.local/state/goro)
`

// runExport は goro export の本体で、終了コードを返す。結果は stdout、診断は stderr。
func runExport(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("goro export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, exportUsage) }
	stateDir := flags.String("state-dir", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "goro export: セッション ID を 1 つ指定する。一覧: goro sessions")
		return exitUsage
	}
	id := flags.Arg(0)
	dir, err := resolveStateDir(*stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "goro export: %v\n", err)
		return 1
	}
	store, err := session.NewStore(dir, bwrap.CurrentHost())
	if err != nil {
		fmt.Fprintf(stderr, "goro export: %v\n", err)
		return 1
	}
	b, err := store.Export(context.Background(), id)
	if err != nil {
		fmt.Fprintf(stderr, "goro export: %v\n", err)
		return 1
	}
	printBundle(stdout, b)
	return 0
}

// printBundle は、bundle の path・ブランチ・取り込みのコマンドを w に出す。
func printBundle(w io.Writer, b *session.Bundle) {
	fmt.Fprintf(w, "bundle: %s (%d バイト)\n", b.Path, b.Size)
	fmt.Fprintf(w, "ブランチ: %s\n", strings.Join(b.Heads, ", "))
	if b.Skipped > 0 {
		fmt.Fprintf(w, "(名前が安全でないため、除いたブランチ: %d)\n", b.Skipped)
	}
	fmt.Fprintf(w, "取り込み (自分の repo で実行する):\n  %s\n", b.Fetch)
}

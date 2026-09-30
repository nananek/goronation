//go:build linux

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nananek/goronation/vault"
)

// vaultUsage は、goronation vault -h の使い方。
const vaultUsage = `使い方: goronation vault reset [--state-dir DIR]

reset   Vault の中身 (passkey のラップ・暗号化した資格情報とログイン状態・Vault の ID) を壊す。
        値は、読まない・使わない (解錠していなくてもよい)。web のログインの passkey と、監査の記録 (<DIR>/vault/audit.log) は、消さない。
        端末からだけ実行できる (対話で、消す Vault の ID の先頭 8 文字を入力して確認する)。
        Vault のデーモンが動いている (lock を持っている) ときは、実行しない。止めてから、もう一度。

  --state-dir DIR   状態を置く場所 (既定は $XDG_STATE_HOME/goronation か ~/.local/state/goronation。Vault は <DIR>/vault)
`

// runVault は goronation vault の本体で、終了コードを返す (成功 0・できない/中止 1・使い方の誤り 2)。サブコマンドは reset だけ。
// reset は、シェルからだけ (stdin が端末であること) で、web の経路からは呼べない (ADR 0034)。
func runVault(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goronation vault: "+format+"\n", a...)
		return 1
	}
	usageErr := func(msg string) int {
		fmt.Fprintf(stderr, "goronation vault: %s。使い方: goronation vault -h\n", msg)
		return exitUsage
	}
	if len(args) == 0 {
		return usageErr("サブコマンドを指定する (reset)")
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Fprint(stderr, vaultUsage)
		return 0
	case "reset":
	default:
		return usageErr("未知のサブコマンド")
	}
	flags := flag.NewFlagSet("goronation vault reset", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	stateDir := flags.String("state-dir", "", "")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, vaultUsage)
			return 0
		}
		return usageErr("引数が不正")
	}
	if flags.NArg() > 0 {
		return usageErr("余計な引数")
	}
	if !isTTYFile(stdin) {
		return fail("reset は、端末からだけ実行できる (対話で確認する)。標準入力が端末でない")
	}
	base, err := resolveStateDir(*stateDir)
	if err != nil {
		return fail("%v", err)
	}
	dir := filepath.Join(base, "vault")
	id, err := vault.PeekID(dir)
	switch {
	case errors.Is(err, vault.ErrNotInitialized):
		return fail("Vault が無い (%s)。消すものはない", dir)
	case err != nil:
		return fail("Vault を確かめられない: %v", err)
	}
	want := "reset"
	if id != "unknown" {
		want = id[:8]
	}
	fmt.Fprintf(stderr, "消す Vault: %s\n  ID: %s\n  passkey のラップ・資格情報・ログイン状態が、全部消える。web のログインの passkey は、残る。\n  Vault に置いた資格情報は、入れ直す (goronation auth)。外部 (GitHub など) に登録した鍵は、別に失効する。\n確認のため、%q を入力: ", dir, id, want)
	line, err := readShortLine(stdin, 64)
	if err != nil || strings.TrimSpace(line) != want {
		return fail("確認が合わない。中止した (何も消していない)")
	}
	gone, err := vault.Reset(dir, vault.ResetOptions{ExpectID: id})
	switch {
	case errors.Is(err, vault.ErrVaultChanged):
		return fail("確認したあとに、Vault が差し替わった。何も消していない。もう一度実行する")
	case errors.Is(err, vault.ErrInUse):
		return fail("Vault のデーモンが動いている (lock を持っている)。止めてから、もう一度実行する (何も消していない)")
	case err != nil:
		return fail("reset できない: %v", err)
	}
	fmt.Fprintf(stdout, "Vault を消した (ID %s)。監査の記録: %s\n", gone, filepath.Join(dir, "audit.log"))
	return 0
}

// readShortLine は、in から 1 行 (max バイトまで) 読む。長すぎる行は error。
func readShortLine(in io.Reader, max int) (string, error) {
	r := bufio.NewReaderSize(in, max+2)
	line, err := r.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	return string(line), nil
}

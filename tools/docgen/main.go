package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const usage = "使い方: docgen [-check] (path の引数は無い。cwd は <root>/tools/docgen)"

// mode は、実行の種類。
type mode int

const (
	modeGenerate mode = iota // 生成物を書く
	modeCheck                // 書かずに、生成物との一致と、形式を検査する
)

func parseArgs(args []string) (mode, error) {
	switch {
	case len(args) == 0:
		return modeGenerate, nil
	case len(args) == 1 && args[0] == "-check":
		return modeCheck, nil
	}
	return 0, errors.New("引数が不正")
}

// emit は、標準出力・標準エラーへ 1 行を出す唯一の関数。normalize を通すので、診断に入る名前や本文の
// 制御文字・改行が、端末の制御列や偽の行として出ない。
func emit(w io.Writer, s string) {
	out, err := normalize(kindDiag, s)
	if err != nil {
		out = "docgen: 診断を出力できない"
	}
	fmt.Fprintln(w, out)
}

func main() {
	// 予算 (limits.Timeout) が効かないところ (書き込みなど) で止まっても、無限には待たない。
	watchdog := time.AfterFunc(defaultLimits.Timeout+10*time.Second, func() {
		emit(os.Stderr, "docgen: 全体の時間の上限を超えたため、強制終了する")
		os.Exit(1)
	})
	code := run(os.Args[1:], os.Stdout, os.Stderr, os.Getwd)
	watchdog.Stop()
	os.Exit(code)
}

// run は docgen の本体。終了コードを返す (0: 成功、1: error か検査の失敗、2: 使い方の誤り)。
func run(args []string, stdout, stderr io.Writer, getwd func() (string, error)) int {
	if _, err := parseArgs(args); err != nil {
		emit(stderr, "docgen: "+err.Error())
		emit(stderr, usage)
		return 2
	}
	cwd, err := getwd()
	if err != nil {
		emit(stderr, "docgen: cwd を得られない: "+err.Error())
		return 1
	}
	root, err := openRepo(cwd)
	if err != nil {
		emit(stderr, "docgen: "+err.Error())
		return 1
	}
	defer root.Close()

	t := newTree(root, defaultLimits)
	inv, err := t.inventory()
	if err != nil {
		emit(stderr, "docgen: "+err.Error())
		return 1
	}
	emit(stdout, fmt.Sprintf("docgen: .go %d 個・go.mod %d 個・.md %d 個を読む (生成は、まだ無い)",
		len(inv.Go), len(inv.Mod), len(inv.Markdown)))
	return 0
}

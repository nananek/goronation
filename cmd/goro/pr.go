//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/goronation/cmd/internal/credfile"
	"github.com/nananek/goronation/cmd/internal/ghapi"
	"github.com/nananek/goronation/core/credential"
	"github.com/nananek/goronation/egress/gateway"
	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

// jailAPIBase は、檻の中から PR 作成の要求を送る先 (goro init が中継する loopback)。goro run --push が、
// egress/gateway をこの経路の先に配線したときだけ、答えが返る。var なのは、テストが偽の上流に差し替えるため
// (本番はこの値のまま)。
var jailAPIBase = "http://" + jailProxyAddr + github.PathPrefix

// prUsage は、goro pr -h の使い方。create (檻の中) と ready (ホスト) の 2 つで、動く場所が違う。
const prUsage = `使い方: goro pr create --title T [--body B] [--base BRANCH]
        goro pr ready OWNER/REPO N [--state-dir DIR]

create: 檻の中から、PR 作成の要求を送る (goro run --push owner/repo で起動した檻の中でだけ動く。
        GORO_PUSH_REPO が無ければ断る)。head は、今いる repo の現在のブランチ。base を省くと、
        repo の既定の branch になる。PR は常に draft で作られる (ready にするのは、ホスト側の
        goro pr ready)。

  --title T   PR の題 (必須)
  --body B    PR の本文 (省略可)
  --base B    base のブランチ名 (省略時は、repo の既定の branch)

ready: ホストから、PR を ready for review にする (draft を外す)。対象 PR の head commit の
       checks (StatusCheckRollup) が success (または、checks が 1 つも無い) であることを確かめて
       から、GitHub の GraphQL API を呼ぶ (gh コマンドは使わない)。checks が終わっていない・
       失敗していれば、何もせず断る。

  OWNER/REPO        対象の repo
  N                 PR の番号
  --state-dir DIR   goro auth github で保存したトークンの場所 (既定は $XDG_STATE_HOME/goro か ~/.local/state/goro)
`

// runPr は goro pr の本体。create・ready へ振り分ける。
func runPr(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, prUsage)
		return exitUsage
	}
	switch args[0] {
	case "create":
		return runPrCreate(args[1:], stdout, stderr)
	case "ready":
		return runPrReady(args[1:], stdout, stderr)
	case "-h", "--help":
		fmt.Fprint(stderr, prUsage)
		return 0
	}
	fmt.Fprintf(stderr, "goro pr: 未知のサブコマンド %q\n%s", args[0], prUsage)
	return exitUsage
}

// prCreateTimeout は、goro pr create が、egress の応答を待つ上限 (檻の中の egress 中継 + 上流の GitHub)。
const prCreateTimeout = 30 * time.Second

// maxPrCreateRespBytes は、goro pr create が読む応答の上限。成功時は {"number":N,"html_url":"…"} の小さい
// JSON、失敗時は http.StatusText の短い文字列 (gateway は、それ以上の詳細を檻に返さない)。
const maxPrCreateRespBytes = 64 << 10

// runPrCreate は goro pr create の本体 (檻の中で動く)。
func runPrCreate(args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro pr create: "+format+"\n", a...)
		return 1
	}
	flags := flag.NewFlagSet("goro pr create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, prUsage) }
	title := flags.String("title", "", "")
	body := flags.String("body", "", "")
	base := flags.String("base", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "goro pr create: 余計な引数 %q\n", flags.Arg(0))
		return exitUsage
	}
	if strings.TrimSpace(*title) == "" {
		fmt.Fprintln(stderr, "goro pr create: --title が要る")
		return exitUsage
	}
	repo := os.Getenv("GORO_PUSH_REPO")
	if repo == "" {
		return fail("GORO_PUSH_REPO が無い (goro run --push owner/repo で起動していない)")
	}
	if _, err := git.ParseRepo(repo); err != nil {
		return fail("GORO_PUSH_REPO %s が owner/repo の形ではない", sanitize(repo))
	}
	head, err := currentBranch(".")
	if err != nil {
		return fail("今のブランチを読めない: %s", sanitize(err.Error()))
	}
	payload := map[string]string{"title": *title, "head": head}
	if *body != "" {
		payload["body"] = *body
	}
	if *base != "" {
		payload["base"] = *base
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return fail("要求を組み立てられない: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), prCreateTimeout)
	defer cancel()
	url := jailAPIBase + "repos/" + repo + "/pulls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fail("要求を作れない: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// http.DefaultClient (http.ProxyFromEnvironment 由来) をそのまま使う: goro init が設定する NO_PROXY に
	// 127.0.0.1 が既に入っているため (檻の中の別プロセスへの直接アクセスを、proxy 経由にしないための既存の
	// 仕組み)、この loopback 宛の要求は、二重に自分自身を proxy として経由せず、直接届く。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail("egress に繋げない: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxPrCreateRespBytes+1))
	if err != nil {
		return fail("応答を読めない: %v", err)
	}
	if len(respBody) > maxPrCreateRespBytes {
		return fail("応答が大きすぎる")
	}
	if resp.StatusCode != http.StatusCreated {
		return fail("PR を作れなかった (状態 %d): %s", resp.StatusCode, sanitize(strings.TrimSpace(string(respBody))))
	}
	var result struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fail("応答を JSON として読めない")
	}
	fmt.Fprintf(stdout, "PR #%d を作った (draft): %s\n", result.Number, sanitize(result.HTMLURL))
	return 0
}

// maxGitHeadBytes は、.git/HEAD を読む量の上限 (本物は 50 バイト前後。檻が書く敵対な中身を大量に読まない)。
const maxGitHeadBytes = 4096

// findGitDir は、dir から上に辿って、.git ディレクトリを持つ最初の場所を探す (git 自身の探し方と同じ)。
func findGitDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for range 40 { // 上限: 際限なく辿らない (symlink のループなどに対する保険)
		fi, err := os.Stat(filepath.Join(abs, ".git"))
		if err == nil && fi.IsDir() {
			return filepath.Join(abs, ".git"), nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return "", fmt.Errorf(".git が見つからない (%s から上を探した)", dir)
}

// currentBranch は、dir (git の作業ツリーの中のどこか) の、今のブランチ名を返す (.git/HEAD を読む。
// detached HEAD なら error)。
func currentBranch(dir string) (string, error) {
	gitDir, err := findGitDir(dir)
	if err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxGitHeadBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxGitHeadBytes {
		return "", errors.New(".git/HEAD が大きすぎる")
	}
	line := strings.TrimSpace(string(b))
	rest, ok := strings.CutPrefix(line, "ref: refs/heads/")
	if !ok || rest == "" {
		return "", errors.New("branch にいない (detached HEAD か、.git/HEAD の形が正しくない)")
	}
	return rest, nil
}

// prReadyTimeout は、goro pr ready が、GitHub の GraphQL API を待つ上限 (要求 2 回: 状態の取得と mutation)。
const prReadyTimeout = 30 * time.Second

// ghapiBaseURL は、goro pr ready が使う GraphQL エンドポイント。var なのは、テストが偽の上流に差し替える
// ため (本番は ghapi.DefaultBaseURL のまま)。
var ghapiBaseURL = ghapi.DefaultBaseURL

// runPrReady は goro pr ready の本体 (ホストで動く)。使い方どおり、OWNER/REPO と N は、オプションより先に書く
// (flag.FlagSet.Parse は、最初の flag でない引数で解釈をやめるため、先に自分で取り出す)。
func runPrReady(args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro pr ready: "+format+"\n", a...)
		return 1
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stderr, prUsage)
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "goro pr ready: OWNER/REPO と PR の番号が要る")
		return exitUsage
	}
	repoArg, numberArg, rest := args[0], args[1], args[2:]

	flags := flag.NewFlagSet("goro pr ready", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, prUsage) }
	stateDir := flags.String("state-dir", "", "")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "goro pr ready: 余計な引数 %q\n", flags.Arg(0))
		return exitUsage
	}
	repo, err := git.ParseRepo(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "goro pr ready: repo %s が owner/repo の形ではない\n", sanitize(repoArg))
		return exitUsage
	}
	number, err := strconv.Atoi(numberArg)
	if err != nil || number < 1 {
		fmt.Fprintf(stderr, "goro pr ready: PR の番号 %s が正しくない\n", sanitize(numberArg))
		return exitUsage
	}
	dir, err := resolveStateDir(*stateDir)
	if err != nil {
		return fail("%v", err)
	}
	store, err := credfile.New(dir)
	if err != nil {
		return fail("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	tctx, cancel := context.WithTimeout(ctx, prReadyTimeout)
	defer cancel()

	token, err := store.Token(tctx, gateway.CredentialName)
	if err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			return fail("トークンが無い。先に: goro auth github")
		}
		return fail("トークンを読めない: %v", err)
	}
	client := &ghapi.Client{Token: token, BaseURL: ghapiBaseURL}
	alreadyReady, err := client.Ready(tctx, repo.Owner, repo.Name, number)
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(stderr, "goro pr ready: 中止した。")
		return 130
	case err != nil:
		return fail("%s", sanitize(err.Error()))
	case alreadyReady:
		fmt.Fprintf(stdout, "%s#%d は、すでに draft ではない\n", repo.String(), number)
	default:
		fmt.Fprintf(stdout, "%s#%d を ready for review にした\n", repo.String(), number)
	}
	return 0
}

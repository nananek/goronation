//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nananek/goronation/tools/fakeproviders/anthropic"
	"github.com/nananek/goronation/tools/fakeproviders/openai"
)

const usage = `使い方: framecapture <opencode|claude> [オプション] -- メッセージ...

M0 スパイク S1 (PR②: frame-capture-harness)。bwrap 檻の中で実物の claude/opencode を、このプロセス
自身が起動する fake provider サーバー (tools/fakeproviders) に向けて動かし、実際のフレームを --out
(既定は標準出力) に採取する。fake サーバーへの到達は、cmd/framecapture/internal/connectproxy (このハーネス
専用の、最小限の CONNECT プロキシ) 経由。egress.Server は使わない (loopback への dial を SSRF 対策で
拒む設計のため。詳細は doc s1-pr2-design)。

  --goronation-bin PATH  goronation 実行ファイル (既定: PATH の goronation)
  --agent-bin PATH       claude/opencode 実行ファイル (既定: PATH の claude/opencode)
  --out PATH             採取したフレームを書くファイル (既定: 標準出力)
  --response TEXT        fake サーバーが返す応答本文 (既定: 決め打ちの文)
  --scenario PATH        場面の JSON (ターン・fake の応答・置くファイル)。指定すると -- のメッセージも --response も要らない
  --normalize            採取したフレームの、実行ごとに変わる値 (ID・時刻・所要時間など) を、固定の記号に置き換えて書く
  --timeout DURATION     檻の実行の期限 (既定 2m。超えたら殺す)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "opencode":
		return runOpencode(args[1:], stdout, stderr)
	case "claude":
		return runClaude(args[1:], stdout, stderr)
	case "-h", "--help":
		fmt.Fprint(stderr, usage)
		return 0
	}
	fmt.Fprintf(stderr, "framecapture: 未知のエージェント %q\n%s", args[0], usage)
	return 2
}

// captureOptions は、opencode・claude で共通のオプション。
type captureOptions struct {
	goronationBin string
	agentBin      string
	out           string
	response      string
	scenario      string
	normalize     bool
	timeout       time.Duration
	message       []string
}

// parseCaptureArgs は、args (-- の前がオプション、後ろがメッセージ) を解釈する。
func parseCaptureArgs(sub string, args []string, stderr io.Writer) (captureOptions, error) {
	var o captureOptions
	head, tail := args, []string(nil)
	if i := indexOf(args, "--"); i >= 0 {
		head, tail = args[:i], args[i+1:]
	}
	flags := flag.NewFlagSet("framecapture "+sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.goronationBin, "goronation-bin", "", "")
	flags.StringVar(&o.agentBin, "agent-bin", "", "")
	flags.StringVar(&o.out, "out", "", "")
	flags.StringVar(&o.response, "response", "フレーム採取ハーネス (cmd/framecapture) の fake サーバーからの応答です。", "")
	flags.StringVar(&o.scenario, "scenario", "", "")
	flags.BoolVar(&o.normalize, "normalize", false, "")
	flags.DurationVar(&o.timeout, "timeout", 2*time.Minute, "")
	if err := flags.Parse(head); err != nil {
		return o, err
	}
	o.message = tail
	if len(o.message) == 0 && o.scenario == "" {
		fmt.Fprintf(stderr, "framecapture %s: メッセージが要る (-- の後ろに書く。--scenario を使うなら不要)\n", sub)
		return o, errors.New("引数が不正")
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

// plan は、1 回の採取の中身: ユーザーが順に送るメッセージと、work に置くファイルと、終了コードの期待値。
type plan struct {
	turns      []string
	sc         *scenario // --scenario が無ければ nil
	expectExit int
}

// newPlan は、o から plan を作る。--scenario があればその turns を、なければ -- のメッセージ (スペースで
// 結合した 1 ターン) を使う。agent は "claude" か "opencode" (期待する終了コードの選択に使う)。
func newPlan(o captureOptions, agent string) (plan, error) {
	if o.scenario == "" {
		return plan{turns: []string{strings.Join(o.message, " ")}}, nil
	}
	sc, err := loadScenario(o.scenario)
	if err != nil {
		return plan{}, err
	}
	p := plan{turns: sc.Turns, sc: sc, expectExit: sc.ExpectExit.Claude}
	if agent == "opencode" {
		p.expectExit = sc.ExpectExit.Opencode
	}
	return p, nil
}

// workSetup は、work の下ごしらえ: エージェント固有 (agentSetup。nil でもよい) のあとに、場面のファイルを置く。
func (p plan) workSetup(agentSetup func(work string) error) func(work string) error {
	return func(work string) error {
		if agentSetup != nil {
			if err := agentSetup(work); err != nil {
				return err
			}
		}
		if p.sc != nil {
			return p.sc.writeFiles(work)
		}
		return nil
	}
}

// emit は、採取した raw を (--normalize なら正規化して) --out か stdout に書く。
func emit(o captureOptions, raw []byte, stdout io.Writer) error {
	if o.normalize {
		var err error
		if raw, err = normalizeFrames(raw); err != nil {
			return fmt.Errorf("フレームを正規化できない: %w", err)
		}
	}
	if o.out == "" {
		_, err := stdout.Write(raw)
		return err
	}
	return os.WriteFile(o.out, raw, 0o644)
}

// finish は、エージェントの終了コード code (と、採取の書き出しの結果) から、framecapture の終了コードを決める。
// 期待した終了コード (エラー応答の場面など) なら、成功 (0) とみなす。期待外なら、その終了コード (0 で終わったなら 1)。
func finish(o captureOptions, name string, p plan, code int, raw []byte, stdout, stderr io.Writer) int {
	if err := emit(o, raw, stdout); err != nil {
		fmt.Fprintf(stderr, "framecapture %s: %v\n", name, err)
		return 1
	}
	switch {
	case code == p.expectExit && len(bytes.TrimSpace(raw)) == 0:
		// 期待した終了コードが 1 のとき、ハーネス自身の失敗 (檻を起こせない等。終了コード 1) と区別できない。
		// フレームが 1 つも採れていないなら、成功とはみなさない。
		fmt.Fprintf(stderr, "framecapture %s: 終了コード %d だが、フレームが 1 つも採れていない\n", name, code)
		return 1
	case code == p.expectExit:
		return 0
	case code == 0:
		fmt.Fprintf(stderr, "framecapture %s: 終了コード 0 で終わったが、%d を期待していた\n", name, p.expectExit)
		return 1
	}
	return code
}

// runOpencode は、framecapture opencode の本体。ターンごとに檻を起こし直し (opencode run は 1 メッセージで
// 終わる)、HOME を持ち越して --continue で同じセッションを続ける。
func runOpencode(args []string, stdout, stderr io.Writer) int {
	o, err := parseCaptureArgs("opencode", args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	p, err := newPlan(o, "opencode")
	if err != nil {
		fmt.Fprintf(stderr, "framecapture opencode: %v\n", err)
		return 2
	}
	steps := []openai.Step{{Content: o.response}}
	if p.sc != nil {
		if steps = p.sc.Opencode; len(steps) == 0 {
			fmt.Fprintf(stderr, "framecapture opencode: %v\n", errNoSteps)
			return 2
		}
	}
	fake := openai.NewServer(steps...)
	newFakeHTTP := func() *http.Server {
		hs := fake.NewHTTPServer()
		hs.Handler = withTitleRequests(hs.Handler)
		return hs
	}

	stateDir, err := os.MkdirTemp("", "framecapture-")
	if err != nil {
		fmt.Fprintf(stderr, "framecapture opencode: 作業ディレクトリを作れない: %v\n", err)
		return 1
	}
	defer os.RemoveAll(stateDir)

	var raw bytes.Buffer
	code := 0
	for i, msg := range p.turns {
		code = runHarness(harnessRunInput{
			AgentName:       "opencode",
			AgentBinFlag:    o.agentBin,
			GoroBinFlag:     o.goronationBin,
			FakeHandler:     newFakeHTTP(),
			FakeVirtualHost: opencodeFakeHost,
			FakeVirtualPort: opencodeFakeVirtualPort,
			Env:             opencodeEnv(),
			Args:            opencodeArgs(i > 0, []string{msg}),
			Out:             &raw,
			StateDir:        stateDir,
			Timeout:         o.timeout,
			WorkSetup:       p.workSetup(writeOpencodeConfig),
		}, stdout, stderr)
		if code != 0 && i < len(p.turns)-1 {
			break // 途中のターンが失敗したら、続けない
		}
	}
	return finish(o, "opencode", p, code, raw.Bytes(), stdout, stderr)
}

// runClaude は、framecapture claude の本体。すべてのターンを、stream-json の 1 行ずつとして標準入力に
// 渡して (1 回の起動で順に処理させ)、標準入力を閉じる。
func runClaude(args []string, stdout, stderr io.Writer) int {
	o, err := parseCaptureArgs("claude", args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	p, err := newPlan(o, "claude")
	if err != nil {
		fmt.Fprintf(stderr, "framecapture claude: %v\n", err)
		return 2
	}
	if p.sc != nil && len(p.sc.ClaudePermissions) > 0 {
		// この場面は、対話 (host モード) の採取: can_use_tool の control_request に答える必要があるので、
		// 全ターンを起動前に書いて stdin を閉じる下の経路 (静的) ではなく、runClaudeInteractive を使う。
		return runClaudeInteractive(o, p, stdout, stderr)
	}
	steps := []anthropic.Step{{Text: o.response}}
	if p.sc != nil {
		if steps = p.sc.Claude; len(steps) == 0 {
			fmt.Fprintf(stderr, "framecapture claude: %v\n", errNoSteps)
			return 2
		}
	}
	fake := anthropic.NewServer(steps...)

	var stdin bytes.Buffer
	for _, msg := range p.turns {
		line, merr := json.Marshal(claudeStreamJSONLine(msg))
		if merr != nil {
			fmt.Fprintf(stderr, "framecapture claude: stdin の JSON を組み立てられない: %v\n", merr)
			return 1
		}
		stdin.Write(line)
		stdin.WriteByte('\n')
	}
	var raw bytes.Buffer
	code := runHarness(harnessRunInput{
		AgentName:       "claude",
		AgentBinFlag:    o.agentBin,
		GoroBinFlag:     o.goronationBin,
		FakeHandler:     fake.NewHTTPServer(),
		FakeVirtualHost: claudeFakeHost,
		FakeVirtualPort: claudeFakeVirtualPort,
		Env:             claudeEnv(),
		Args:            claudeArgs(),
		Stdin:           &stdin,
		Out:             &raw,
		Timeout:         o.timeout,
		WorkSetup:       p.workSetup(nil),
	}, stdout, stderr)
	return finish(o, "claude", p, code, raw.Bytes(), stdout, stderr)
}

//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nananek/goronation/tools/fakeproviders/anthropic"
	"github.com/nananek/goronation/tools/fakeproviders/openai"
)

const usage = `使い方: framecapture <opencode|claude> [オプション] -- メッセージ...

M0 スパイク S1 (PR②: frame-capture-harness)。bwrap 檻の中で実物の claude/opencode を、このプロセス
自身が起動する fake provider サーバー (tools/fakeproviders) に向けて動かし、実際のフレームを --out
(既定は標準出力) に採取する。fake サーバーへの到達は、cmd/framecapture/connectproxy (このハーネス
専用の、最小限の CONNECT プロキシ) 経由。egress.Server は使わない (loopback への dial を SSRF 対策で
拒む設計のため。詳細は doc s1-pr2-design)。

  --goronation-bin PATH  goronation 実行ファイル (既定: PATH の goronation)
  --agent-bin PATH       claude/opencode 実行ファイル (既定: PATH の claude/opencode)
  --out PATH             採取したフレームを書くファイル (既定: 標準出力)
  --response TEXT        fake サーバーが返す応答本文 (既定: 決め打ちの文)
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
	if err := flags.Parse(head); err != nil {
		return o, err
	}
	o.message = tail
	if len(o.message) == 0 {
		fmt.Fprintf(stderr, "framecapture %s: メッセージが要る (-- の後ろに書く)\n", sub)
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

// runOpencode は、framecapture opencode の本体。
func runOpencode(args []string, stdout, stderr io.Writer) int {
	o, err := parseCaptureArgs("opencode", args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fake := openai.NewServer(openai.Step{Content: o.response})
	return runHarness(harnessRunInput{
		AgentName:       "opencode",
		AgentBinFlag:    o.agentBin,
		GoroBinFlag:     o.goronationBin,
		FakeHandler:     fake.NewHTTPServer(),
		FakeVirtualHost: opencodeFakeHost,
		FakeVirtualPort: opencodeFakeVirtualPort,
		Env:             opencodeEnv(),
		Args:            opencodeArgs("", o.message),
		Out:             o.out,
		WorkSetup:       writeOpencodeConfig,
	}, stdout, stderr)
}

// runClaude は、framecapture claude の本体。message は 1 つの stream-json ターンとして標準入力に渡す
// (複数要素の message は、スペースで結合して 1 ターンにする)。
func runClaude(args []string, stdout, stderr io.Writer) int {
	o, err := parseCaptureArgs("claude", args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fake := anthropic.NewServer(anthropic.Step{Text: o.response})
	line, merr := json.Marshal(claudeStreamJSONLine(strings.Join(o.message, " ")))
	if merr != nil {
		fmt.Fprintf(stderr, "framecapture claude: stdin の JSON を組み立てられない: %v\n", merr)
		return 1
	}
	return runHarness(harnessRunInput{
		AgentName:       "claude",
		AgentBinFlag:    o.agentBin,
		GoroBinFlag:     o.goronationBin,
		FakeHandler:     fake.NewHTTPServer(),
		FakeVirtualHost: claudeFakeHost,
		FakeVirtualPort: claudeFakeVirtualPort,
		Env:             claudeEnv(),
		Args:            claudeArgs(),
		Stdin:           strings.NewReader(string(line) + "\n"),
		Out:             o.out,
	}, stdout, stderr)
}

//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// goronation mcp は、檻の中で動く MCP (Model Context Protocol) のサーバー。エージェント (claude・opencode) に
// goronation run --push の使い方を、起動のたびに説明しなくて済むように create_pr・push_context を出し、組み込みの
// bash/Bash tool が managed 設定で deny された後の、唯一の実行経路として run_command を出す (check_status のような、
// egress の新しい読み取り専用の経路が要るものは、ここには入れない。別の変更)。
// pr ready (draft を外す) は出さない: それは、より強い権限を持つ、将来の別の主体のための経路として空けてある。
// run_command は、push の有無に関わらず要る (bash/Bash の deny 自体が push の有無を問わないため)。mcpServersFor が
// goronation 自身を常に登録するのは、このため。
//
// 標準入出力は、MCP の stdio transport の規約どおり: 標準入力から、改行区切りの JSON-RPC 2.0 を読み、標準
// 出力には、その応答だけを書く (ログ・診断は標準エラーへ。標準出力に他のものを混ぜると、エージェント側の
// JSON-RPC の parsing が壊れる)。goronation mcp は、資格情報を持たない (I2 は、goronation run --push が、トークンを檻に
// 渡さないことで、すでに守られている)。この tool の返り値・エラーは、そのままエージェント (檻の中の同じ
// プロセス) に渡ってよいものだけにする: ホストの path・トークンは、扱わない・組み立てない。

// mcpProtocolVersion は、initialize の応答に載せる、goronation mcp が話す MCP のプロトコル版。
const mcpProtocolVersion = "2025-06-18"

// mcpServerVersion は、initialize の応答の serverInfo.version。tool の形 (名前・引数) が変わったときだけ上げる。
const mcpServerVersion = "1"

// maxMCPLineBytes は、標準入力の 1 行 (1 メッセージ) の上限。tools/call の引数 (PR の本文など) を十分に
// 許しつつ、際限のない読み込みを避ける。
const maxMCPLineBytes = 1 << 20 // 1 MiB

// mcpServerDef は、エージェントに注入する MCP サーバー 1 つの定義。
type mcpServerDef struct {
	Name    string   // MCP サーバーの登録名
	Command string   // 実行ファイル (檻の中の絶対 path)
	Args    []string // 引数
}

// mcpInjector は、servers を、エージェントの起動時の引数・環境変数に変換する関数。エージェントごとの違いは、
// この関数だけに閉じる (cage.go・run.go は、エージェントの種類で分岐しない: agentProfile.mcp を呼ぶだけ)。
type mcpInjector func(servers []mcpServerDef) (args []string, env []bwrap.EnvVar)

// mcpServerName は、goronation 自身を MCP サーバーとして登録するときの名前。
const mcpServerName = "goronation"

// mcpServersFor は、goronation 自身 (goronation mcp、引数は "mcp" だけ) を注入する MCP サーバーの一覧にする。
// 常に 1 つ登録する (push の有無に関わらず): run_command が、bash/Bash を managed 設定で deny した後の、唯一の
// 実行経路になるため。push 専用の tool (create_pr・push_context) は、--push が無いときに呼ばれても、mcp.go の
// mcpCreatePR・mcpPushContext が error を返すだけ。ファイルへ保存する状態は無く、起動のたび (resume を含む) に、
// この関数から作り直すので、常に今の形になる。
func mcpServersFor() []mcpServerDef {
	return []mcpServerDef{{Name: mcpServerName, Command: jailGoro, Args: []string{"mcp"}}}
}

// claudeMCPInject は、claude の --mcp-config '<JSON>' (起動時の引数。プロセスに閉じ、~/.claude.json は
// 書き換えない) に変換する。形は、claude --help の記述と、実機 (claude 2.1.283) での実行で確かめた。
func claudeMCPInject(servers []mcpServerDef) (args []string, env []bwrap.EnvVar) {
	if len(servers) == 0 {
		return nil, nil
	}
	type entry struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args,omitempty"`
	}
	m := make(map[string]entry, len(servers))
	for _, s := range servers {
		m[s.Name] = entry{Type: "stdio", Command: s.Command, Args: s.Args}
	}
	b, err := json.Marshal(struct {
		MCPServers map[string]entry `json:"mcpServers"`
	}{MCPServers: m})
	if err != nil {
		return nil, nil // m は固定の構造 (文字列だけ) なので、実際には起こらない
	}
	return []string{"--mcp-config", string(b)}, nil
}

// opencodeMCPInject は、opencode の環境変数 OPENCODE_CONFIG_CONTENT (起動時の inline JSON。project の設定に
// deep-merge され、ファイルは書かない) に変換する。opencode の実機での検証はできていない (このホストに実物の
// バイナリが無い): 形は、公式ドキュメント (https://opencode.ai/docs/mcp-servers/、
// https://opencode.ai/docs/config/#custom) の記述から確かめた。
func opencodeMCPInject(servers []mcpServerDef) (args []string, env []bwrap.EnvVar) {
	if len(servers) == 0 {
		return nil, nil
	}
	type entry struct {
		Type    string   `json:"type"`
		Command []string `json:"command"`
	}
	m := make(map[string]entry, len(servers))
	for _, s := range servers {
		m[s.Name] = entry{Type: "local", Command: append([]string{s.Command}, s.Args...)}
	}
	b, err := json.Marshal(struct {
		Schema string           `json:"$schema"`
		MCP    map[string]entry `json:"mcp"`
	}{Schema: "https://opencode.ai/config.json", MCP: m})
	if err != nil {
		return nil, nil
	}
	return nil, []bwrap.EnvVar{{Key: "OPENCODE_CONFIG_CONTENT", Value: string(b)}}
}

// JSON-RPC 2.0 (MCP のメッセージの形)。ID は、通知 (応答しない) かどうかの判定に使うだけなので、そのまま
// json.RawMessage で持ち、応答にそのまま載せて返す (数値でも文字列でも、送られてきた形のまま)。
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC の標準のエラー番号 (MCP はこれをそのまま使う)。
const (
	mcpParseError     = -32700
	mcpMethodNotFound = -32601
	mcpInvalidParams  = -32602
)

// runMCP は goronation mcp の本体 (檻の中で動く)。標準入力を読み終える (EOF) か、読めなくなるまで応答し続ける。
func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "goronation mcp: 引数は取らない (エージェントの MCP 設定から、標準入出力で呼ばれる)")
		return exitUsage
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMCPLineBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		resp := handleMCPLine(line)
		if resp == nil {
			continue // 通知 (id 無し) か、通知にした応答: 何も書かない
		}
		b, err := json.Marshal(resp)
		if err != nil {
			continue // mcpResponse は固定の構造なので、実際には起こらない
		}
		if _, err := fmt.Fprintln(stdout, string(b)); err != nil {
			fmt.Fprintf(stderr, "goronation mcp: 標準出力に書けない: %v\n", err)
			return 1
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "goronation mcp: 標準入力を読めない: %v\n", err)
		return 1
	}
	return 0
}

// handleMCPLine は、1 行 (1 つの JSON-RPC メッセージ) を処理し、応答を返す。通知 (id が無い要求。応答して
// はいけない) には nil を返す。
func handleMCPLine(line []byte) *mcpResponse {
	var req mcpRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpRPCError{Code: mcpParseError, Message: "壊れた JSON"}}
	}
	isNotification := len(req.ID) == 0
	respond := func(result any) *mcpResponse {
		if isNotification {
			return nil
		}
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	fail := func(code int, msg string) *mcpResponse {
		if isNotification {
			return nil
		}
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpRPCError{Code: code, Message: msg}}
	}
	switch req.Method {
	case "initialize":
		return respond(mcpInitializeResult())
	case "notifications/initialized", "notifications/cancelled":
		return nil // 通知: 何もしない
	case "ping":
		return respond(struct{}{})
	case "tools/list":
		return respond(struct {
			Tools []mcpToolDef `json:"tools"`
		}{Tools: mcpTools()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(mcpInvalidParams, "壊れた params")
		}
		result, ok := callMCPTool(p.Name, p.Arguments)
		if !ok {
			return fail(mcpInvalidParams, "未知の tool")
		}
		return respond(result)
	default:
		return fail(mcpMethodNotFound, "未知の method")
	}
}

// mcpInitializeResult は、initialize の応答。tools 以外の機能 (resources・prompts) は出さない。
func mcpInitializeResult() any {
	return struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    any    `json:"capabilities"`
		ServerInfo      any    `json:"serverInfo"`
	}{
		ProtocolVersion: mcpProtocolVersion,
		Capabilities: struct {
			Tools struct{} `json:"tools"`
		}{},
		ServerInfo: struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}{Name: "goronation", Version: mcpServerVersion},
	}
}

// mcpToolDef は、tools/list に載せる tool 1 つの定義。
type mcpToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// mcpTools は、goronation mcp が出す tool の一覧: create_pr・push_context・run_command (pr ready は出さない。
// ファイル先頭の doc comment を参照)。
func mcpTools() []mcpToolDef {
	return []mcpToolDef{
		{
			Name: "run_command",
			Description: "シェルコマンド (/bin/sh -c) を実行する。組み込みの bash/Bash tool の代わり (このエージェントでは、" +
				"managed 設定で deny されている)。標準出力・標準エラーを合わせたものと、終了コードを返す。実行は、" +
				"goronation mcp 自身の直接の子プロセスとして行う (別の名前空間には分離しない)。呼び出しは、監査ログに残る。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "実行するコマンド (シェルの 1 行)"},
					"timeout_seconds": map[string]any{
						"type": "number",
						"description": fmt.Sprintf("省略時は %d 秒。上限は %d 秒 (超えると打ち切る)",
							int(runCommandDefaultTimeout.Seconds()), int(runCommandMaxTimeout.Seconds())),
					},
				},
				"required": []string{"command"},
			},
		},
		{
			Name: "create_pr",
			Description: "今のブランチから、draft の PR を作る (goronation run --push owner/repo で起動しているときだけ動く)。" +
				"base を省くと repo の既定の branch になる。PR は常に draft で作られ、ready for review にするのは、この MCP の外の役目。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{"type": "string", "description": "PR の題 (必須)"},
					"body":  map[string]any{"type": "string", "description": "PR の本文 (省略可)"},
					"base":  map[string]any{"type": "string", "description": "base のブランチ名 (省略時は repo の既定の branch)"},
				},
				"required": []string{"title"},
			},
		},
		{
			Name:        "push_context",
			Description: "push できる repo・今のブランチ・push できる ref の接頭辞を返す (読み取り専用。ネットワークへは繋がない)。",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// mcpToolCallResult は、tools/call の結果 (MCP の形: content の配列と、tool 自体の失敗を示す isError)。
type mcpToolCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mcpText(s string) []mcpContent { return []mcpContent{{Type: "text", Text: s}} }

func mcpErrorResult(err error) mcpToolCallResult {
	return mcpToolCallResult{Content: mcpText(err.Error()), IsError: true}
}

// callMCPTool は、tool 名 name の呼び出しを実行する。未知の tool 名は ok=false (呼び出し元が、プロトコルの
// エラー (Invalid params) にする。tool 自体の失敗 (PR を作れなかった、など) は、ok=true・IsError=true で返す:
// MCP は、この 2 つを区別する)。
func callMCPTool(name string, args json.RawMessage) (result mcpToolCallResult, ok bool) {
	switch name {
	case "create_pr":
		return mcpCreatePR(args), true
	case "push_context":
		return mcpPushContext(args), true
	case "run_command":
		return mcpRunCommand(args), true
	}
	return mcpToolCallResult{}, false
}

// mcpCreatePR は、create_pr tool。goronation pr create (pr.go) と同じ createPR を呼ぶ: 組み立て・送信・応答の
// 読み取りは 1 つだけ (CLI と MCP で重複させない)。
func mcpCreatePR(args json.RawMessage) mcpToolCallResult {
	var p struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Base  string `json:"base"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpErrorResult(fmt.Errorf("arguments を読めない: %v", err))
		}
	}
	if strings.TrimSpace(p.Title) == "" {
		return mcpErrorResult(errors.New("title が要る"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), prCreateTimeout)
	defer cancel()
	result, err := createPR(ctx, p.Title, p.Body, p.Base)
	if err != nil {
		return mcpErrorResult(err)
	}
	return mcpToolCallResult{Content: mcpText(fmt.Sprintf("PR #%d を作った (draft): %s", result.Number, result.HTMLURL))}
}

// mcpPushContext は、push_context tool。GORONATION_PUSH_REPO・GORONATION_PUSH_REF_PREFIX と、今のブランチを返すだけ
// (ネットワークへは繋がない)。エージェントが、create_pr の base やブランチ名を、毎回説明されなくても
// 組み立てられるようにするための、読み取り専用の手がかり。
func mcpPushContext(args json.RawMessage) mcpToolCallResult {
	repo := os.Getenv("GORONATION_PUSH_REPO")
	if repo == "" {
		return mcpErrorResult(errors.New("GORONATION_PUSH_REPO が無い (goronation run --push owner/repo で起動していない)"))
	}
	branch, err := currentBranch(".")
	if err != nil {
		return mcpErrorResult(fmt.Errorf("今のブランチを読めない: %v", err))
	}
	payload := struct {
		Repo          string `json:"repo"`
		Branch        string `json:"branch"`
		PushRefPrefix string `json:"push_ref_prefix"`
	}{Repo: repo, Branch: branch, PushRefPrefix: os.Getenv("GORONATION_PUSH_REF_PREFIX")}
	b, err := json.Marshal(payload)
	if err != nil {
		return mcpErrorResult(err)
	}
	return mcpToolCallResult{Content: mcpText(string(b))}
}

// runCommandDefaultTimeout・runCommandMaxTimeout は、run_command の実行時間の既定値と上限 (timeout_seconds に
// 上限より大きい値を指定しても、上限で打ち切る)。
const (
	runCommandDefaultTimeout = 120 * time.Second
	runCommandMaxTimeout     = 600 * time.Second
)

// maxRunCommandOutputBytes は、run_command の応答に含める、標準出力・標準エラーを合わせた出力の上限 (それを
// 超える分は捨てる。誘導されたコマンドの出力で、MCP の応答を際限なく大きくしないため)。
const maxRunCommandOutputBytes = 256 * 1024

// mcpRunCommand は run_command tool。bash/Bash が managed 設定で deny された、このエージェントの、唯一の実行
// 経路になる (Phase 1: goronation mcp 自身の直接の子として実行するだけで、入れ子の檻による名前空間の分離は、
// まだ無い。別の PR (Phase 2) で、より狭い bwrap の中に移す)。呼び出しは、すべて logRunCommand が監査ログに残す。
func mcpRunCommand(args json.RawMessage) mcpToolCallResult {
	var p struct {
		Command        string  `json:"command"`
		TimeoutSeconds float64 `json:"timeout_seconds"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpErrorResult(fmt.Errorf("arguments を読めない: %v", err))
		}
	}
	if strings.TrimSpace(p.Command) == "" {
		return mcpErrorResult(errors.New("command が要る"))
	}
	timeout := runCommandDefaultTimeout
	if p.TimeoutSeconds > 0 {
		timeout = time.Duration(p.TimeoutSeconds * float64(time.Second))
	}
	if timeout > runCommandMaxTimeout {
		timeout = runCommandMaxTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", p.Command)
	out := &capturedOutput{max: maxRunCommandOutputBytes}
	cmd.Stdout, cmd.Stderr = out, out
	// command が子プロセスを作る (パイプ・sleep など) と、タイムアウトで殺すのが起動した /bin/sh だけでは、
	// 孫プロセスが標準出力・標準エラーの書き込み端を持ったまま生き残り、Wait がそれらの exit まで (この関数の
	// timeout を超えて) 戻らない。新しい process group で起動し、Cancel (ctx の締切で呼ばれる) で group ごと
	// 殺す。WaitDelay は、それでも応答しないもの (シグナルを無視する孫プロセスなど) がいたときの保険。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	elapsed := time.Since(start)
	timedOut := ctx.Err() == context.DeadlineExceeded

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	logRunCommand(p.Command, exitCode, elapsed, timedOut)

	switch {
	case timedOut:
		return mcpToolCallResult{Content: mcpText(fmt.Sprintf("タイムアウト (%s) で打ち切った\n%s", timeout, out.String())), IsError: true}
	case cmd.ProcessState == nil: // 起動自体に失敗した (シェル自体が無い、など。command 自体の失敗は、正常な終了コードで返す)
		return mcpErrorResult(fmt.Errorf("実行できない: %v", runErr))
	default:
		return mcpToolCallResult{Content: mcpText(fmt.Sprintf("終了コード: %d\n%s", exitCode, out.String())), IsError: exitCode != 0}
	}
}

// capturedOutput は、標準出力・標準エラーを合わせて受け、最大 max バイトまで保つ io.Writer (それ以降は、数だけ
// 数えて捨てる)。Write は常に成功を返す: コマンドの実行自体を、出力の量で失敗させない。
type capturedOutput struct {
	max     int
	buf     bytes.Buffer
	dropped int64
}

func (c *capturedOutput) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		n := room
		if n > len(p) {
			n = len(p)
		}
		c.buf.Write(p[:n])
		c.dropped += int64(len(p) - n)
	} else {
		c.dropped += int64(len(p))
	}
	return len(p), nil
}

func (c *capturedOutput) String() string {
	s := c.buf.String()
	if c.dropped > 0 {
		s += fmt.Sprintf("\n... (出力の残り %d バイトを切り捨てた)", c.dropped)
	}
	return s
}

// runCommandLogEntry は、runCommandLogPath に追記する 1 行 (JSON) の形。
type runCommandLogEntry struct {
	Time     string  `json:"time"`
	Command  string  `json:"command"`
	ExitCode int     `json:"exit_code"`
	Seconds  float64 `json:"seconds"`
	TimedOut bool    `json:"timed_out,omitempty"`
}

// runCommandLogPath は、run_command 1 回分の監査ログを追記する path: 今の HOME (repo ごと。ホストからは、
// state dir の下の、repo ごとの HOME の中として見える) の下の隠しディレクトリ。HOME が絶対 path でなければ
// (テストなど)、空を返す (呼び手は、書き込みを諦める)。
func runCommandLogPath() string {
	home := os.Getenv("HOME")
	if !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".goronation", "run-command.log")
}

// logRunCommand は、run_command 1 回分を、runCommandLogPath に追記する。書けなくても、tool の応答には影響しない
// (監査は付随の効果で、実行そのものを妨げない)。
func logRunCommand(command string, exitCode int, elapsed time.Duration, timedOut bool) {
	path := runCommandLogPath()
	if path == "" {
		return
	}
	b, err := json.Marshal(runCommandLogEntry{
		Time: time.Now().UTC().Format(time.RFC3339Nano), Command: command, ExitCode: exitCode,
		Seconds: elapsed.Seconds(), TimedOut: timedOut,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

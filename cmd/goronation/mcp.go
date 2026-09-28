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
	"strings"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// goro mcp は、檻の中で動く MCP (Model Context Protocol) のサーバー。エージェント (claude・opencode) に
// goro run --push の使い方を、起動のたびに説明しなくて済むように、create_pr・push_context の 2 つの tool を
// 出す (check_status のような、egress の新しい読み取り専用の経路が要るものは、ここには入れない。別の変更)。
// pr ready (draft を外す) は出さない: それは、より強い権限を持つ、将来の別の主体のための経路として空けてある。
//
// 標準入出力は、MCP の stdio transport の規約どおり: 標準入力から、改行区切りの JSON-RPC 2.0 を読み、標準
// 出力には、その応答だけを書く (ログ・診断は標準エラーへ。標準出力に他のものを混ぜると、エージェント側の
// JSON-RPC の parsing が壊れる)。goro mcp は、資格情報を持たない (I2 は、goro run --push が、トークンを檻に
// 渡さないことで、すでに守られている)。この tool の返り値・エラーは、そのままエージェント (檻の中の同じ
// プロセス) に渡ってよいものだけにする: ホストの path・トークンは、扱わない・組み立てない。

// mcpProtocolVersion は、initialize の応答に載せる、goro mcp が話す MCP のプロトコル版。
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

// mcpServerName は、goro 自身を MCP サーバーとして登録するときの名前。
const mcpServerName = "goro"

// mcpServersFor は、--push が有効なときだけ、goro 自身 (goro mcp、引数は "mcp" だけ) を注入する MCP サーバー
// の一覧にする (push が無いと、create_pr も push_context も動かないので、出しても使えない)。ファイルへ保存
// する状態は無く、--push が有効な起動のたび (resume を含む) に、この関数から作り直すので、常に今の形になる。
func mcpServersFor(push string) []mcpServerDef {
	if push == "" {
		return nil
	}
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

// runMCP は goro mcp の本体 (檻の中で動く)。標準入力を読み終える (EOF) か、読めなくなるまで応答し続ける。
func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "goro mcp: 引数は取らない (エージェントの MCP 設定から、標準入出力で呼ばれる)")
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
			fmt.Fprintf(stderr, "goro mcp: 標準出力に書けない: %v\n", err)
			return 1
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "goro mcp: 標準入力を読めない: %v\n", err)
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
		}{Name: "goro", Version: mcpServerVersion},
	}
}

// mcpToolDef は、tools/list に載せる tool 1 つの定義。
type mcpToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// mcpTools は、goro mcp が出す tool の一覧: create_pr・push_context だけ (pr ready は出さない。ファイル
// 先頭の doc comment を参照)。
func mcpTools() []mcpToolDef {
	return []mcpToolDef{
		{
			Name: "create_pr",
			Description: "今のブランチから、draft の PR を作る (goro run --push owner/repo で起動しているときだけ動く)。" +
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
	}
	return mcpToolCallResult{}, false
}

// mcpCreatePR は、create_pr tool。goro pr create (pr.go) と同じ createPR を呼ぶ: 組み立て・送信・応答の
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

// mcpPushContext は、push_context tool。GORO_PUSH_REPO・GORO_PUSH_REF_PREFIX と、今のブランチを返すだけ
// (ネットワークへは繋がない)。エージェントが、create_pr の base やブランチ名を、毎回説明されなくても
// 組み立てられるようにするための、読み取り専用の手がかり。
func mcpPushContext(args json.RawMessage) mcpToolCallResult {
	repo := os.Getenv("GORO_PUSH_REPO")
	if repo == "" {
		return mcpErrorResult(errors.New("GORO_PUSH_REPO が無い (goro run --push owner/repo で起動していない)"))
	}
	branch, err := currentBranch(".")
	if err != nil {
		return mcpErrorResult(fmt.Errorf("今のブランチを読めない: %v", err))
	}
	payload := struct {
		Repo          string `json:"repo"`
		Branch        string `json:"branch"`
		PushRefPrefix string `json:"push_ref_prefix"`
	}{Repo: repo, Branch: branch, PushRefPrefix: os.Getenv("GORO_PUSH_REF_PREFIX")}
	b, err := json.Marshal(payload)
	if err != nil {
		return mcpErrorResult(err)
	}
	return mcpToolCallResult{Content: mcpText(string(b))}
}

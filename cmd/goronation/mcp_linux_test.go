//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestMCPServersForGatedOnPush(t *testing.T) {
	if got := mcpServersFor(""); got != nil {
		t.Errorf("mcpServersFor(\"\") = %v, want nil (--push が無ければ登録しない)", got)
	}
	got := mcpServersFor("o/r")
	want := []mcpServerDef{{Name: "goronation", Command: jailGoro, Args: []string{"mcp"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mcpServersFor(\"o/r\") = %+v, want %+v", got, want)
	}
}

func TestClaudeMCPInject(t *testing.T) {
	if args, env := claudeMCPInject(nil); args != nil || env != nil {
		t.Errorf("空の servers で args/env が出た: %v %v", args, env)
	}
	args, env := claudeMCPInject([]mcpServerDef{{Name: "goronation", Command: "/opt/goronation/goronation", Args: []string{"mcp"}}})
	if env != nil {
		t.Errorf("claude は起動時の環境変数を使わない: %v", env)
	}
	if len(args) != 2 || args[0] != "--mcp-config" {
		t.Fatalf("args = %v, want [--mcp-config <json>]", args)
	}
	want := `{"mcpServers":{"goronation":{"type":"stdio","command":"/opt/goronation/goronation","args":["mcp"]}}}`
	if args[1] != want {
		t.Errorf("--mcp-config の JSON = %s, want %s", args[1], want)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args[1]), &probe); err != nil {
		t.Fatalf("--mcp-config が JSON でない: %v", err)
	}
}

func TestOpencodeMCPInject(t *testing.T) {
	if args, env := opencodeMCPInject(nil); args != nil || env != nil {
		t.Errorf("空の servers で args/env が出た: %v %v", args, env)
	}
	args, env := opencodeMCPInject([]mcpServerDef{{Name: "goronation", Command: "/opt/goronation/goronation", Args: []string{"mcp"}}})
	if args != nil {
		t.Errorf("opencode は起動時の引数を使わない: %v", args)
	}
	if len(env) != 1 || env[0].Key != "OPENCODE_CONFIG_CONTENT" {
		t.Fatalf("env = %v, want [OPENCODE_CONFIG_CONTENT=<json>]", env)
	}
	want := `{"$schema":"https://opencode.ai/config.json","mcp":{"goronation":{"type":"local","command":["/opt/goronation/goronation","mcp"]}}}`
	if env[0].Value != want {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %s, want %s", env[0].Value, want)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(env[0].Value), &probe); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT が JSON でない: %v", err)
	}
}

// TestCageSpecInjectsMCPForClaude は、--push が有効なとき、claude の argv に --mcp-config が、
// エージェントの実体の直後・利用者の引数 (--resume など) より前に入ることを確かめる。
func TestCageSpecInjectsMCPForClaude(t *testing.T) {
	c := testCage()
	c.MCPServers = mcpServersFor("o/r")
	argv, err := cageSpec(c).Argv()
	if err != nil {
		t.Fatal(err)
	}
	// /opt/claude/claude は、--ro-bind の宛先 (bind 元・宛先) にも出るので、最後の出現 (Cmd の中の、実際に
	// 実行するコマンド) を使う。
	i := -1
	for j, a := range argv {
		if a == "/opt/claude/claude" {
			i = j
		}
	}
	if i < 0 {
		t.Fatal("argv に /opt/claude/claude が無い")
	}
	if argv[i+1] != "--mcp-config" {
		t.Fatalf("エージェントの直後 = %q, want --mcp-config: argv = %q", argv[i+1], argv)
	}
	if !strings.Contains(argv[i+2], `"goronation"`) || !strings.Contains(argv[i+2], "/opt/goronation/goronation") {
		t.Errorf("--mcp-config の値がおかしい: %q", argv[i+2])
	}
	if argv[i+3] != "--resume" { // testCage().Args = {"--resume", "x y"}
		t.Errorf("利用者の引数が、--mcp-config の後ろに続いていない: %q", argv[i+3:])
	}
}

// TestCageSpecNoMCPWithoutPush は、--push が無効 (MCPServers が空) なら、claude の argv に --mcp-config が
// 一切出ないことを確かめる (これまでの golden (TestCageSpecGolden) と同じ形のまま)。
func TestCageSpecNoMCPWithoutPush(t *testing.T) {
	argv, err := cageSpec(testCage()).Argv()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range argv {
		if a == "--mcp-config" {
			t.Fatalf("--push なしなのに --mcp-config が argv にある: %q", argv)
		}
	}
}

// TestCageSpecInjectsMCPForOpenCode は、--push が有効なとき、opencode の環境変数に
// OPENCODE_CONFIG_CONTENT が足されることを確かめる (opencode は引数でなく環境変数)。
func TestCageSpecInjectsMCPForOpenCode(t *testing.T) {
	c := testOpenCodeCage()
	c.MCPServers = mcpServersFor("o/r")
	spec := cageSpec(c)
	found := false
	for _, e := range spec.Env {
		if e.Key == "OPENCODE_CONFIG_CONTENT" {
			found = true
			if !strings.Contains(e.Value, "/opt/goronation/goronation") {
				t.Errorf("OPENCODE_CONFIG_CONTENT = %s", e.Value)
			}
		}
	}
	if !found {
		t.Fatal("OPENCODE_CONFIG_CONTENT が環境変数に無い")
	}
	argv, err := spec.Argv()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range argv {
		if a == "--mcp-config" {
			t.Fatalf("opencode の argv に、claude 用の --mcp-config が混ざっている: %q", argv)
		}
	}
}

// mcpRoundTrip は、req (JSON-RPC の 1 メッセージ) を runMCP に送り、応答 (無ければ nil) を返す。
func mcpRoundTrip(t *testing.T, req string) map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	in := strings.NewReader(req + "\n")
	if code := runMCP(nil, in, &out, &errOut); code != 0 {
		t.Fatalf("runMCP code = %d, stderr = %q", code, errOut.String())
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		return nil
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("応答が JSON でない: %v (%q)", err, line)
	}
	return resp
}

func TestRunMCPInitialize(t *testing.T) {
	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result が無い: %+v", resp)
	}
	if result["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
	si, ok := result["serverInfo"].(map[string]any)
	if !ok || si["name"] != "goronation" {
		t.Errorf("serverInfo = %+v", result["serverInfo"])
	}
}

func TestRunMCPNotificationHasNoResponse(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	if code := runMCP(nil, in, &out, &errOut); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("通知に応答した: %q", out.String())
	}
}

func TestRunMCPUnknownMethod(t *testing.T) {
	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":2,"method":"nope"}`)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error が無い: %+v", resp)
	}
	if int(errObj["code"].(float64)) != mcpMethodNotFound {
		t.Errorf("code = %v", errObj["code"])
	}
}

func TestRunMCPMalformedJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader("{not json}\n")
	if code := runMCP(nil, in, &out, &errOut); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("応答が JSON でない: %v (%q)", err, out.String())
	}
	if _, ok := resp["error"]; !ok {
		t.Fatalf("error が無い: %+v", resp)
	}
}

func TestRunMCPRejectsExtraArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runMCP([]string{"extra"}, strings.NewReader(""), &out, &errOut); code != exitUsage {
		t.Fatalf("code = %d, want exitUsage", code)
	}
}

func TestRunMCPToolsList(t *testing.T) {
	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result が無い: %+v", resp)
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %+v, want 2 件", result["tools"])
	}
	names := map[string]bool{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		names[m["name"].(string)] = true
	}
	if !names["create_pr"] || !names["push_context"] {
		t.Errorf("tools の名前 = %v, want create_pr・push_context", names)
	}
	if names["pr_ready"] {
		t.Error("pr_ready を出してはいけない (将来の、より強い権限の主体向けに空けてある)")
	}
}

func TestRunMCPToolsCallCreatePR(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, "ref: refs/heads/goronation/sess-1/x\n")
	chdir(t, dir)
	t.Setenv("GORONATION_PUSH_REPO", "o/r")

	var gotBody map[string]any
	withFakeJailAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":9,"html_url":"https://github.com/o/r/pull/9"}`)
	})

	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_pr","arguments":{"title":"t","body":"b"}}}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result が無い: %+v", resp)
	}
	if result["isError"] == true {
		t.Fatalf("isError = true: %+v", result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %+v", result["content"])
	}
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "#9") || !strings.Contains(text, "pull/9") {
		t.Errorf("text = %q", text)
	}
	if gotBody["title"] != "t" || gotBody["head"] != "goronation/sess-1/x" {
		t.Errorf("上流に送った body = %+v", gotBody)
	}
}

func TestRunMCPToolsCallCreatePRMissingTitleIsToolError(t *testing.T) {
	t.Setenv("GORONATION_PUSH_REPO", "o/r")
	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"create_pr","arguments":{}}}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("プロトコルのエラーではなく、tool の結果 (isError) であるべき: %+v", resp)
	}
	if result["isError"] != true {
		t.Errorf("isError = %v, want true", result["isError"])
	}
}

func TestRunMCPToolsCallUnknownToolIsProtocolError(t *testing.T) {
	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error が無い: %+v", resp)
	}
	if int(errObj["code"].(float64)) != mcpInvalidParams {
		t.Errorf("code = %v", errObj["code"])
	}
}

func TestRunMCPToolsCallPushContext(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, "ref: refs/heads/goronation/sess-1/x\n")
	chdir(t, dir)
	t.Setenv("GORONATION_PUSH_REPO", "o/r")
	t.Setenv("GORONATION_PUSH_REF_PREFIX", "refs/heads/goronation/sess-1/")

	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"push_context","arguments":{}}}`)
	result, ok := resp["result"].(map[string]any)
	if !ok || result["isError"] == true {
		t.Fatalf("result = %+v", resp)
	}
	content := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload map[string]string
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("push_context の text が JSON でない: %v (%q)", err, content)
	}
	if payload["repo"] != "o/r" || payload["branch"] != "goronation/sess-1/x" || payload["push_ref_prefix"] != "refs/heads/goronation/sess-1/" {
		t.Errorf("push_context = %+v", payload)
	}
}

func TestRunMCPToolsCallPushContextWithoutPush(t *testing.T) {
	t.Setenv("GORONATION_PUSH_REPO", "")
	resp := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"push_context","arguments":{}}}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result が無い: %+v", resp)
	}
	if result["isError"] != true {
		t.Errorf("isError = %v, want true (GORONATION_PUSH_REPO が無い)", result["isError"])
	}
}

// TestRunMCPMultipleMessages は、標準入力に複数行あるとき、それぞれに順に応答することを確かめる
// (bufio.Scanner が、改行区切りのまま扱うこと)。
func TestRunMCPMultipleMessages(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n",
	)
	if code := runMCP(nil, in, &out, &errOut); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	sc := bufio.NewScanner(&out)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 2 {
		t.Fatalf("応答が %d 行, want 2: %q", len(lines), out.String())
	}
}

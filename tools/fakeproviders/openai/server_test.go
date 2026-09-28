package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postChatCompletions(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// dataFrame は、テストが読んだ 1 件の SSE data 行。
type dataFrame struct {
	raw  string
	data map[string]any // raw == "[DONE]" のときは nil
}

func readDataLines(t *testing.T, body []byte) []dataFrame {
	t.Helper()
	var frames []dataFrame
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		if raw == "[DONE]" {
			frames = append(frames, dataFrame{raw: raw})
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("data 行が JSON でない: %v (%q)", err, line)
		}
		frames = append(frames, dataFrame{raw: raw, data: v})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return frames
}

func readAll(resp *http.Response) ([]byte, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

func choiceDelta(t *testing.T, f dataFrame) map[string]any {
	t.Helper()
	choices := f.data["choices"].([]any)
	choice := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	return delta
}

func TestNonStreamingContent(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Content: "こんにちは"}))
	defer s.Close()

	resp := postChatCompletions(t, s, `{"model":"fake-gpt","stream":false,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "こんにちは" || got.Choices[0].Message.Role != "assistant" {
		t.Fatalf("choices = %+v", got.Choices)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
}

func TestStreamingContent(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Content: "hello"}))
	defer s.Close()

	resp := postChatCompletions(t, s, `{"model":"fake-gpt","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	frames := readDataLines(t, body)
	// role チャンク → content チャンク → finish_reason チャンク → [DONE]
	if len(frames) != 4 {
		t.Fatalf("frames = %d, want 4 (%+v)", len(frames), frames)
	}
	if frames[3].raw != "[DONE]" {
		t.Fatalf("last frame = %q, want [DONE]", frames[3].raw)
	}
	roleDelta := choiceDelta(t, frames[0])
	if roleDelta["role"] != "assistant" {
		t.Fatalf("frame[0].delta = %+v", roleDelta)
	}
	contentDelta := choiceDelta(t, frames[1])
	if contentDelta["content"] != "hello" {
		t.Fatalf("frame[1].delta = %+v", contentDelta)
	}
	choices := frames[2].data["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
	}
}

func TestNonStreamingToolCalls(t *testing.T) {
	step := Step{
		ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)}},
	}
	s := httptest.NewServer(NewServer(step))
	defer s.Close()

	resp := postChatCompletions(t, s, `{"model":"fake-gpt","stream":false,"messages":[]}`)
	defer resp.Body.Close()
	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg := got.Choices[0].Message
	if msg.Content != nil {
		t.Fatalf("content = %v, want null (ToolCalls のみで Content が空のとき)", *msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_1" || msg.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool_calls = %+v", msg.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(msg.ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments が JSON でない: %v", err)
	}
	if args["path"] != "a.txt" {
		t.Fatalf("args = %+v", args)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
}

func TestStreamingToolCalls(t *testing.T) {
	step := Step{
		ToolCalls: []ToolCall{{ID: "call_2", Name: "read_file", Arguments: json.RawMessage(`{"path":"b.txt"}`)}},
	}
	s := httptest.NewServer(NewServer(step))
	defer s.Close()

	resp := postChatCompletions(t, s, `{"model":"fake-gpt","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	frames := readDataLines(t, body)
	// role → tool_calls(id/name) → tool_calls(arguments) → finish_reason → [DONE]
	if len(frames) != 5 {
		t.Fatalf("frames = %d, want 5 (%+v)", len(frames), frames)
	}
	nameDelta := choiceDelta(t, frames[1])
	calls := nameDelta["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_2" {
		t.Fatalf("frame[1] tool_calls = %+v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "read_file" {
		t.Fatalf("frame[1] function = %+v", fn)
	}

	argsDelta := choiceDelta(t, frames[2])
	argsCalls := argsDelta["tool_calls"].([]any)
	argsCall := argsCalls[0].(map[string]any)
	argsFn := argsCall["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal([]byte(argsFn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments が JSON でない: %v", err)
	}
	if args["path"] != "b.txt" {
		t.Fatalf("args = %+v", args)
	}
}

func TestMultiTurnAdvancesAndRepeatsLastStep(t *testing.T) {
	s := httptest.NewServer(NewServer(
		Step{Content: "first"},
		Step{Content: "second"},
	))
	defer s.Close()

	want := []string{"first", "second", "second", "second"}
	for i, w := range want {
		resp := postChatCompletions(t, s, `{"model":"fake-gpt","stream":false,"messages":[]}`)
		var got struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("call %d: decode: %v", i, err)
		}
		resp.Body.Close()
		if len(got.Choices) != 1 || got.Choices[0].Message.Content != w {
			t.Fatalf("call %d: content = %+v, want %q", i, got.Choices, w)
		}
	}
}

func TestErrorStep(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Error: &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Message: "busy"}}))
	defer s.Close()

	resp := postChatCompletions(t, s, `{"model":"fake-gpt","stream":false,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var got struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Type != "rate_limit_error" || got.Error.Message != "busy" {
		t.Fatalf("got = %+v", got)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Content: "x"}))
	defer s.Close()

	resp, err := http.Get(s.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/chat/completions: status = %d, want 404", resp.StatusCode)
	}

	resp2, err := http.Post(s.URL+"/v1/other", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /v1/other: status = %d, want 404", resp2.StatusCode)
	}
}

func TestRequestsAreCaptured(t *testing.T) {
	s := NewServer(Step{Content: "x"})
	srv := httptest.NewServer(s)
	defer srv.Close()

	postChatCompletions(t, srv, `{"model":"fake-gpt","stream":false,"messages":[{"role":"user","content":"hi"}]}`).Body.Close()
	postChatCompletions(t, srv, `{"model":"fake-gpt","stream":false,"messages":[]}`).Body.Close()

	got := s.Requests()
	if len(got) != 2 {
		t.Fatalf("len(Requests()) = %d, want 2", len(got))
	}
	var body map[string]any
	if err := json.Unmarshal(got[0].Body, &body); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}
	if body["model"] != "fake-gpt" {
		t.Fatalf("captured body = %+v", body)
	}
}

// TestSlowBodyTimesOut は、本文を送り切らずに接続だけ繋ぎ続ける (slow-body) 接続で、ハンドラの goroutine が
// 無期限にブロックされない (攻撃者視点レビューの finding 2 の再現・回帰確認) ことを確かめる。
func TestSlowBodyTimesOut(t *testing.T) {
	old := readBodyTimeout
	readBodyTimeout = 100 * time.Millisecond
	defer func() { readBodyTimeout = old }()

	s := httptest.NewServer(NewServer(Step{Content: "x"}))
	defer s.Close()

	addr := strings.TrimPrefix(s.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Content-Length を大きく宣言しつつ、本文の一部だけを送り、残りは送らない。
	req := "POST /v1/chat/completions HTTP/1.1\r\nHost: " + addr +
		"\r\nContent-Type: application/json\r\nContent-Length: 100000000\r\n\r\n" + `{"model":"x"`
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf) // readBodyTimeout を過ぎて接続が閉じられれば、エラー (EOF 等) で戻る
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("slow-body 接続が readBodyTimeout を過ぎてもハングし続けた (read timeout が効いていない)")
	}
}

// TestSlowHeaderDoesNotHangForever は、finding 2 の姉妹脆弱性 (finding 4) の再現・回帰確認: リクエスト
// ヘッダーを送り切らずに接続だけ繋ぎ続ける接続 (本文ではなくヘッダー自体が未完成) は、ServeHTTP 内の
// SetReadDeadline (readBodyTimeout) では防げない (ハンドラはヘッダー解析が終わるまで呼ばれないため)。
// httptest.NewServer(handler) には ReadHeaderTimeout を設定する手段が無いので、ここでは
// httptest.NewUnstartedServer + NewHTTPServer (ReadHeaderTimeout 込み) を使う。
func TestSlowHeaderDoesNotHangForever(t *testing.T) {
	old := readHeaderTimeout
	readHeaderTimeout = 100 * time.Millisecond
	defer func() { readHeaderTimeout = old }()

	s := httptest.NewUnstartedServer(nil)
	s.Config = NewServer(Step{Content: "x"}).NewHTTPServer()
	s.Start()
	defer s.Close()

	addr := strings.TrimPrefix(s.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// リクエストラインとヘッダーの一部だけを送り、ヘッダー終端の空行を送らない。
	partial := "POST /v1/chat/completions HTTP/1.1\r\nHost: " + addr + "\r\nContent-Type: application/json\r\n"
	if _, err := conn.Write([]byte(partial)); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ヘッダーを送り切らない接続が5秒経ってもハンドラに到達せず応答/切断が無い " +
			"(ReadHeaderTimeout が無いため、readBodyTimeout の修正とは無関係に無期限にハングしうる)")
	}
}

func TestNewServerPanicsWithoutSteps(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer() (steps 無し) は panic するはず")
		}
	}()
	NewServer()
}

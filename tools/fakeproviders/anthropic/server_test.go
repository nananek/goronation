package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseFrame は、テストが読んだ 1 件の SSE イベント (event 行 + data 行)。
type sseFrame struct {
	event string
	data  map[string]any
}

// readSSE は、SSE の応答本体を、空行区切りのイベント列として読む。
func readSSE(t *testing.T, body []byte) []sseFrame {
	t.Helper()
	var frames []sseFrame
	sc := bufio.NewScanner(bytes.NewReader(body))
	var cur sseFrame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				frames = append(frames, cur)
			}
			cur = sseFrame{}
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var v map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Fatalf("data 行が JSON でない: %v (%q)", err, line)
			}
			cur.data = v
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return frames
}

func postMessages(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestNonStreamingText(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Text: "こんにちは"}))
	defer s.Close()

	resp := postMessages(t, s, `{"model":"claude-fake","stream":false,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "こんにちは" {
		t.Fatalf("content = %+v", got.Content)
	}
	if got.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got.StopReason)
	}
}

func TestStreamingText(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Text: "hello"}))
	defer s.Close()

	resp := postMessages(t, s, `{"model":"claude-fake","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	frames := readSSE(t, body)

	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("frames = %d, want %d (%+v)", len(frames), len(wantEvents), frames)
	}
	for i, want := range wantEvents {
		if frames[i].event != want {
			t.Errorf("frame[%d].event = %q, want %q", i, frames[i].event, want)
		}
	}
	delta := frames[2].data["delta"].(map[string]any)
	if delta["text"] != "hello" {
		t.Errorf("text delta = %v, want hello", delta["text"])
	}
	msgDelta := frames[4].data["delta"].(map[string]any)
	if msgDelta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msgDelta["stop_reason"])
	}
}

func TestStreamingToolUse(t *testing.T) {
	step := Step{
		ToolUse: &ToolUse{ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)},
	}
	s := httptest.NewServer(NewServer(step))
	defer s.Close()

	resp := postMessages(t, s, `{"model":"claude-fake","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	frames := readSSE(t, body)

	// ToolUse だけ (Text 無し) のときは text block を送らない: content_block_start/delta/stop (tool_use) が続く。
	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("frames = %d, want %d (%+v)", len(frames), len(wantEvents), frames)
	}
	block := frames[1].data["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "toolu_1" || block["name"] != "read_file" {
		t.Fatalf("content_block = %+v", block)
	}
	delta := frames[2].data["delta"].(map[string]any)
	if delta["type"] != "input_json_delta" {
		t.Fatalf("delta.type = %v", delta["type"])
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(delta["partial_json"].(string)), &input); err != nil {
		t.Fatalf("partial_json が JSON でない: %v", err)
	}
	if input["path"] != "a.txt" {
		t.Fatalf("input = %+v", input)
	}
	msgDelta := frames[4].data["delta"].(map[string]any)
	if msgDelta["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use (既定)", msgDelta["stop_reason"])
	}
}

func TestNonStreamingToolUse(t *testing.T) {
	step := Step{
		Text:    "ファイルを読みます",
		ToolUse: &ToolUse{ID: "toolu_2", Name: "read_file", Input: json.RawMessage(`{"path":"b.txt"}`)},
	}
	s := httptest.NewServer(NewServer(step))
	defer s.Close()

	resp := postMessages(t, s, `{"model":"claude-fake","stream":false,"messages":[]}`)
	defer resp.Body.Close()
	var got struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content = %+v, want 2 blocks", got.Content)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "ファイルを読みます" {
		t.Errorf("content[0] = %+v", got.Content[0])
	}
	if got.Content[1].Type != "tool_use" || got.Content[1].ID != "toolu_2" || got.Content[1].Name != "read_file" {
		t.Errorf("content[1] = %+v", got.Content[1])
	}
	if got.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", got.StopReason)
	}
}

func TestMultiTurnAdvancesAndRepeatsLastStep(t *testing.T) {
	s := httptest.NewServer(NewServer(
		Step{Text: "first"},
		Step{Text: "second"},
	))
	defer s.Close()

	want := []string{"first", "second", "second", "second"}
	for i, w := range want {
		resp := postMessages(t, s, `{"model":"claude-fake","stream":false,"messages":[]}`)
		var got struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("call %d: decode: %v", i, err)
		}
		resp.Body.Close()
		if len(got.Content) != 1 || got.Content[0].Text != w {
			t.Fatalf("call %d: text = %+v, want %q", i, got.Content, w)
		}
	}
}

func TestErrorStep(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Error: &Error{Status: http.StatusTooManyRequests, Type: "overloaded_error", Message: "busy"}}))
	defer s.Close()

	resp := postMessages(t, s, `{"model":"claude-fake","stream":false,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != "error" || got.Error.Type != "overloaded_error" || got.Error.Message != "busy" {
		t.Fatalf("got = %+v", got)
	}
}

func TestErrorStepAlsoAppliesWhenStreaming(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Error: &Error{Status: http.StatusInternalServerError, Message: "boom"}}))
	defer s.Close()

	resp := postMessages(t, s, `{"model":"claude-fake","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json (エラーは stream=true でも JSON のまま)", ct)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	s := httptest.NewServer(NewServer(Step{Text: "x"}))
	defer s.Close()

	resp, err := http.Get(s.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/messages: status = %d, want 404", resp.StatusCode)
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
	s := NewServer(Step{Text: "x"})
	srv := httptest.NewServer(s)
	defer srv.Close()

	postMessages(t, srv, `{"model":"claude-fake","stream":false,"messages":[{"role":"user","content":"hi"}]}`).Body.Close()
	postMessages(t, srv, `{"model":"claude-fake","stream":false,"messages":[]}`).Body.Close()

	got := s.Requests()
	if len(got) != 2 {
		t.Fatalf("len(Requests()) = %d, want 2", len(got))
	}
	var body map[string]any
	if err := json.Unmarshal(got[0].Body, &body); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}
	if body["model"] != "claude-fake" {
		t.Fatalf("captured body = %+v", body)
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

func readAll(resp *http.Response) ([]byte, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

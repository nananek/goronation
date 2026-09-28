package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// readBodyTimeout は、POST /v1/chat/completions の本文を読み取る期限。本文を送り切らずに接続だけ繋ぎ続ける
// (slow-body) 接続が、ハンドラの goroutine を無期限にブロックしないためのもの (攻撃者視点レビューで、
// 期限が無いとハングすることを確認済み)。var なのは、テストがこの値を縮めるため。
var readBodyTimeout = 10 * time.Second

// Step は、POST /v1/chat/completions への 1 回の呼び出しに対する応答を 1 つ記述する。
type Step struct {
	// Content は、assistant メッセージの本文。ToolCalls があり、Content が空文字列なら、
	// content は JSON の null で送る (本物の挙動に合わせる)。
	Content string
	// ToolCalls があれば、この Step は tool 呼び出しを含む応答になる。
	ToolCalls []ToolCall
	// FinishReason は choices[0].finish_reason。空なら、ToolCalls があるとき "tool_calls"、
	// なければ "stop"。
	FinishReason string
	// Error があれば、この Step は成功応答の代わりにこのエラーを返す (他のフィールドは無視する)。
	Error *Error
}

// ToolCall は、1 件の function tool_call。
type ToolCall struct {
	ID   string
	Name string
	// Arguments は、完成した JSON (妥当な JSON であること)。OpenAI の形式どおり、応答では
	// JSON エンコードした文字列 (arguments フィールド) として送る。不正な JSON を渡すと panic する。
	Arguments json.RawMessage
}

// Error は、成功応答の代わりに返す、OpenAI 形式のエラー。
type Error struct {
	// Status は HTTP ステータス。0 なら 500。
	Status int
	// Type は OpenAI のエラー種別 (例: "invalid_request_error")。空なら "api_error"。
	Type    string
	Code    string
	Message string
}

// CapturedRequest は、受信した 1 回のリクエストの記録 (テスト・デバッグ用)。
type CapturedRequest struct {
	Header http.Header
	Body   json.RawMessage
}

// Server は、Step を呼び出し順に消費する fake OpenAI 互換 Chat Completions API。ゼロ値は使わない
// (NewServer で作る)。http.Handler を満たすので、httptest.NewServer にそのまま渡せる。
type Server struct {
	mu       sync.Mutex
	steps    []Step
	calls    int
	captured []CapturedRequest
}

// NewServer は、渡した steps を呼び出し順に消費する Server を作る。呼び出し回数が steps の数を超えたら、
// 最後の Step を繰り返す。steps は 1 つ以上必要 (呼び出し側の設定ミスなので panic する)。
func NewServer(steps ...Step) *Server {
	if len(steps) == 0 {
		panic("fakeproviders/openai: steps は 1 つ以上必要")
	}
	return &Server{steps: steps}
}

// Requests は、これまでに受信したリクエストのコピーを、受信順に返す。
func (s *Server) Requests() []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CapturedRequest, len(s.captured))
	copy(out, s.captured)
	return out
}

func (s *Server) capture(header http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, CapturedRequest{
		Header: header.Clone(),
		Body:   append(json.RawMessage(nil), body...),
	})
}

// next は、この呼び出しで消費する Step を返す (呼び出し番号 0 起点も併せて返す。応答内の id の生成に使う)。
func (s *Server) next() (int, Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.calls++
	return i, s.steps[i]
}

// ServeHTTP は POST /v1/chat/completions だけを扱う。他の path・method は 404。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	// SetReadDeadline が効かない ResponseWriter (この package の使い方では起きない) では、無視して従来どおり
	// 進む (期限を設けられないだけで、ハンドラ自体は壊れない)。
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(readBodyTimeout))
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.capture(r.Header, body)

	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	n, step := s.next()
	if step.Error != nil {
		writeError(w, step.Error)
		return
	}
	if req.Stream {
		writeStream(w, n, req.Model, step)
		return
	}
	writeCompletion(w, n, req.Model, step)
}

func writeError(w http.ResponseWriter, e *Error) {
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	errType := e.Type
	if errType == "" {
		errType = "api_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var code any
	if e.Code != "" {
		code = e.Code
	}
	mustEncode(w, map[string]any{
		"error": map[string]any{
			"type":    errType,
			"message": e.Message,
			"param":   nil,
			"code":    code,
		},
	})
}

func writeCompletion(w http.ResponseWriter, n int, model string, step Step) {
	w.Header().Set("Content-Type", "application/json")
	completionTokens := outputTokens(step)
	mustEncode(w, map[string]any{
		"id":      completionID(n),
		"object":  "chat.completion",
		"created": now(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message(step),
			"finish_reason": finishReason(step),
		}},
		"usage": map[string]int{
			"prompt_tokens":     10,
			"completion_tokens": completionTokens,
			"total_tokens":      10 + completionTokens,
		},
	})
}

func message(step Step) map[string]any {
	m := map[string]any{"role": "assistant", "content": contentValue(step)}
	if len(step.ToolCalls) > 0 {
		m["tool_calls"] = toolCallsJSON(step.ToolCalls)
	}
	return m
}

func contentValue(step Step) any {
	if step.Content == "" && len(step.ToolCalls) > 0 {
		return nil
	}
	return step.Content
}

func toolCallsJSON(calls []ToolCall) []map[string]any {
	out := make([]map[string]any, len(calls))
	for i, c := range calls {
		args := c.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		out[i] = map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": string(mustCanonicalJSON(args)),
			},
		}
	}
	return out
}

// mustCanonicalJSON は、raw が妥当な JSON であることを確かめて返す (不正なら panic)。
// マーシャルはしない: 元の byte 列をそのまま arguments 文字列にする。
func mustCanonicalJSON(raw json.RawMessage) json.RawMessage {
	if !json.Valid(raw) {
		panic("fakeproviders/openai: ToolCall.Arguments が妥当な JSON でない")
	}
	return raw
}

// writeStream は、req.Stream == true のときの応答を、OpenAI の chat.completion.chunk 列として書く
// (role チャンク → content または tool_calls チャンク → finish_reason チャンクの順)。末尾は
// "data: [DONE]"。
func writeStream(w http.ResponseWriter, n int, model string, step Step) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id := completionID(n)
	created := now()

	writeChunk(w, id, created, model, map[string]any{"role": "assistant"}, nil)
	if step.Content != "" {
		writeChunk(w, id, created, model, map[string]any{"content": step.Content}, nil)
	}
	for i, c := range step.ToolCalls {
		args := c.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		writeChunk(w, id, created, model, map[string]any{
			"tool_calls": []map[string]any{{
				"index": i, "id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": ""},
			}},
		}, nil)
		writeChunk(w, id, created, model, map[string]any{
			"tool_calls": []map[string]any{{
				"index":    i,
				"function": map[string]any{"arguments": string(mustCanonicalJSON(args))},
			}},
		}, nil)
	}
	if flusher != nil {
		flusher.Flush()
	}

	reason := finishReason(step)
	writeChunk(w, id, created, model, map[string]any{}, &reason)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeChunk(w io.Writer, id string, created int64, model string, delta map[string]any, finishReason *string) {
	var fr any
	if finishReason != nil {
		fr = *finishReason
	}
	b, err := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0, "delta": delta, "finish_reason": fr,
		}},
	})
	if err != nil {
		// delta はこの package が組み立てた固定の形か、ToolCall.Arguments (妥当な JSON という契約) だけなので、
		// ここに来るのは呼び出し側が不正な JSON を渡した設定ミス。
		panic(fmt.Sprintf("fakeproviders/openai: %v", err))
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func mustEncode(w io.Writer, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(fmt.Sprintf("fakeproviders/openai: %v", err))
	}
}

func finishReason(step Step) string {
	if step.FinishReason != "" {
		return step.FinishReason
	}
	if len(step.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// outputTokens は、実際のトークン数ではなく、それらしい大きさの数を作るだけの粗い見積もり。
func outputTokens(step Step) int {
	n := len(step.Content) / 4
	for _, c := range step.ToolCalls {
		n += len(c.Arguments) / 4
	}
	if n == 0 {
		n = 1
	}
	return n
}

func completionID(n int) string {
	return fmt.Sprintf("chatcmpl-fake-%03d", n)
}

func now() int64 { return time.Now().Unix() }

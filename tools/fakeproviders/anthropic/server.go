package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// readBodyTimeout は、POST /v1/messages の本文を読み取る期限。本文を送り切らずに接続だけ繋ぎ続ける
// (slow-body) 接続が、ハンドラの goroutine を無期限にブロックしないためのもの (攻撃者視点レビューで、
// 期限が無いとハングすることを確認済み)。var なのは、テストがこの値を縮めるため。
var readBodyTimeout = 10 * time.Second

// Step は、POST /v1/messages への 1 回の呼び出しに対する応答を 1 つ記述する。
type Step struct {
	// Text は、text content block の本文。ToolUse が nil のときは、空文字列でも常にこの block を送る。
	Text string
	// ToolUse があれば、Text の content block (Text が空文字列でも送る) に続けて、tool_use content block を送る。
	ToolUse *ToolUse
	// StopReason は、message_delta.delta.stop_reason に入れる。空なら、ToolUse があるとき "tool_use"、
	// なければ "end_turn"。
	StopReason string
	// Error があれば、この Step は成功応答の代わりにこのエラーを返す (Text・ToolUse・StopReason は無視する)。
	Error *Error
}

// ToolUse は、tool_use content block の中身。
type ToolUse struct {
	ID   string
	Name string
	// Input は、完成した JSON (妥当な JSON であること。streaming 時は、server 側が単一の
	// input_json_delta として送る)。不正な JSON を渡すと、応答の組み立て時に panic する。
	Input json.RawMessage
}

// Error は、成功応答の代わりに返す、Anthropic 形式のエラー。
type Error struct {
	// Status は HTTP ステータス。0 なら 500。
	Status int
	// Type は Anthropic のエラー種別 (例: "overloaded_error"・"invalid_request_error")。空なら "api_error"。
	Type    string
	Message string
}

// CapturedRequest は、受信した 1 回のリクエストの記録 (テスト・デバッグ用)。
type CapturedRequest struct {
	Header http.Header
	Body   json.RawMessage
}

// Server は、Step を呼び出し順に消費する fake Anthropic Messages API。ゼロ値は使わない (NewServer で作る)。
// http.Handler を満たすので、httptest.NewServer にそのまま渡せる。
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
		panic("fakeproviders/anthropic: steps は 1 つ以上必要")
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

// ServeHTTP は POST /v1/messages だけを扱う。他の path・method は 404。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
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
	writeMessage(w, n, req.Model, step)
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
	mustEncode(w, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": e.Message,
		},
	})
}

func writeMessage(w http.ResponseWriter, n int, model string, step Step) {
	w.Header().Set("Content-Type", "application/json")
	mustEncode(w, map[string]any{
		"id":            messageID(n),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       contentBlocks(step),
		"stop_reason":   stopReason(step),
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": 10, "output_tokens": outputTokens(step)},
	})
}

// writeStream は、req.Stream == true のときの応答を、Anthropic の SSE イベント列 (message_start →
// content_block_start/delta/stop (block ごと) → message_delta → message_stop) として書く。
func writeStream(w http.ResponseWriter, n int, model string, step Step) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id := messageID(n)
	writeEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 0},
		},
	})
	if flusher != nil {
		flusher.Flush()
	}

	idx := 0
	if step.ToolUse == nil || step.Text != "" {
		writeEvent(w, "content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		writeEvent(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "text_delta", "text": step.Text},
		})
		writeEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	if step.ToolUse != nil {
		input := step.ToolUse.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		writeEvent(w, "content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{
				"type": "tool_use", "id": step.ToolUse.ID, "name": step.ToolUse.Name, "input": map[string]any{},
			},
		})
		writeEvent(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
		})
		writeEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	if flusher != nil {
		flusher.Flush()
	}

	writeEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(step), "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": outputTokens(step)},
	})
	writeEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}

func writeEvent(w io.Writer, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		// data はこの package が組み立てた固定の形か、Step.ToolUse.Input (妥当な JSON という契約) だけなので、
		// ここに来るのは呼び出し側が不正な JSON を渡した設定ミス。
		panic(fmt.Sprintf("fakeproviders/anthropic: %v", err))
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func mustEncode(w io.Writer, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(fmt.Sprintf("fakeproviders/anthropic: %v", err))
	}
}

func contentBlocks(step Step) []map[string]any {
	var blocks []map[string]any
	if step.ToolUse == nil || step.Text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": step.Text})
	}
	if step.ToolUse != nil {
		input := step.ToolUse.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": step.ToolUse.ID, "name": step.ToolUse.Name, "input": input,
		})
	}
	return blocks
}

func stopReason(step Step) string {
	if step.StopReason != "" {
		return step.StopReason
	}
	if step.ToolUse != nil {
		return "tool_use"
	}
	return "end_turn"
}

// outputTokens は、実際のトークン数ではなく、それらしい大きさの数を作るだけの粗い見積もり。
func outputTokens(step Step) int {
	n := len(step.Text) / 4
	if step.ToolUse != nil {
		n += len(step.ToolUse.Input) / 4
	}
	if n == 0 {
		n = 1
	}
	return n
}

func messageID(n int) string {
	return fmt.Sprintf("msg_fake_%03d", n)
}

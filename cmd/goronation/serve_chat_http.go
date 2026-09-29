//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// chatMaxBody は、書き込み API の本文の上限 (バイト)。text の上限 (chat.MaxMessageBytes) の、JSON のエスケープ (最大 6 倍) 込みの分。
const chatMaxBody = 6*chat.MaxMessageBytes + 1024

// chatMaxSSE は、同時に繋がる SSE の数の上限 (超過は 503)。web が持つ上限とは別の、この serve の最後の砦。
const chatMaxSSE = 32

// chatSSEWriteTimeout は、SSE の 1 回の書き込みに許す時間。読まない相手が、書き込みを、無期限に止めない。
const chatSSEWriteTimeout = 30 * time.Second

// chatHandler は、goronation serve --chat の UDS の HTTP API (認証なし。UDS に繋がれること自体が信頼境界):
//
//	GET  /events      SSE。event: hello (first_seq・generation) → バッファ → ライブ (data: は Event の JSON 1 行) → event: end (exit)
//	POST /message     {"text"}
//	POST /permission  {"generation","request_id","outcome"}  (generation は必須。欠落 400・別の起動 409)
//	POST /stop        {} (本文なし可)
//
// 書き込みの本文は、上限つき・厳格な JSON (未知のフィールド・後ろの余分な値は 400)。エラーは {"error":"<コード>"}。
// 世代なしの承認の経路は無い (chat.Conversation の resolve は非公開)。
type chatHandler struct {
	s   *chatSession
	sse atomic.Int32
}

func newChatHandler(s *chatSession) http.Handler {
	h := &chatHandler{s: s}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", h.events)
	mux.HandleFunc("POST /message", h.message)
	mux.HandleFunc("POST /permission", h.permission)
	mux.HandleFunc("POST /stop", h.stop)
	mux.HandleFunc("GET /version", handleVersion)
	return mux
}

// decodeStrict は、本文 (Content-Type: application/json・上限つき) を、v に厳格に読む。本文なし (allowEmpty のとき) は通す。
func decodeStrict(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" || !allowEmpty {
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		if strings.ToLower(strings.TrimSpace(ct)) != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "content_type")
			return false
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, chatMaxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 && allowEmpty {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // 後ろに、別の値・ゴミがある
		writeError(w, http.StatusBadRequest, "bad_json")
		return false
	}
	return true
}

func (h *chatHandler) message(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text *string `json:"text"`
	}
	if !decodeStrict(w, r, &req, false) {
		return
	}
	if req.Text == nil {
		writeError(w, http.StatusBadRequest, "text_required")
		return
	}
	chatResult(w, h.s.Chat.Conv.Send(*req.Text))
}

func (h *chatHandler) permission(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Generation string `json:"generation"`
		RequestID  string `json:"request_id"`
		Outcome    string `json:"outcome"`
	}
	if !decodeStrict(w, r, &req, false) {
		return
	}
	if req.Generation == "" || req.RequestID == "" || req.Outcome == "" {
		writeError(w, http.StatusBadRequest, "generation_request_id_outcome_required")
		return
	}
	chatResult(w, h.s.Chat.Conv.ResolveIn(req.Generation, req.RequestID, req.Outcome))
}

func (h *chatHandler) stop(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !decodeStrict(w, r, &req, true) {
		return
	}
	h.s.Chat.Conv.Stop()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeResult は、chat.Conversation の error を、status とコードにする。
func chatResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, chat.ErrEmptyText), errors.Is(err, chat.ErrBadText), errors.Is(err, chat.ErrBadOutcome), errors.Is(err, chat.ErrNoGeneration):
		writeError(w, http.StatusBadRequest, "bad_request")
	case errors.Is(err, chat.ErrTextTooLong):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
	case errors.Is(err, chat.ErrUnknownRequest):
		writeError(w, http.StatusNotFound, "unknown_request")
	case errors.Is(err, chat.ErrBusy), errors.Is(err, chat.ErrClosed), errors.Is(err, chat.ErrAlreadyResolved), errors.Is(err, chat.ErrStaleGeneration):
		code := "busy"
		switch {
		case errors.Is(err, chat.ErrClosed):
			code = "closed"
		case errors.Is(err, chat.ErrAlreadyResolved):
			code = "already_resolved"
		case errors.Is(err, chat.ErrStaleGeneration):
			code = "stale_generation"
		}
		writeError(w, http.StatusConflict, code)
	default:
		writeError(w, http.StatusInternalServerError, "internal")
	}
}

// events は、SSE。data: は、Event の JSON (改行を含まない) の 1 行。
func (h *chatHandler) events(w http.ResponseWriter, r *http.Request) {
	if h.sse.Add(1) > chatMaxSSE {
		h.sse.Add(-1)
		writeError(w, http.StatusServiceUnavailable, "too_many_streams")
		return
	}
	defer h.sse.Add(-1)
	sub, err := h.s.Chat.Hub.Subscribe()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "too_many_subscribers")
		return
	}
	defer sub.Close()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, data []byte) bool {
		// SSE の行の区切りは \n・\r・\r\n だけ。JSON (json.Marshal) は、これらを必ずエスケープするが、届いた値を信じずに確かめる
		// (含む Event を、そのまま書くと、別のイベントを偽造されうる)。
		if bytes.ContainsAny(data, "\r\n") {
			return true // 捨てる
		}
		rc.SetWriteDeadline(time.Now().Add(chatSSEWriteTimeout))
		var buf bytes.Buffer
		if event != "" {
			fmt.Fprintf(&buf, "event: %s\n", event)
		}
		buf.WriteString("data: ")
		buf.Write(data)
		buf.WriteString("\n\n")
		if _, err := w.Write(buf.Bytes()); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	hello, _ := json.Marshal(map[string]any{"first_seq": sub.FirstSeq, "generation": h.s.Chat.Conv.Generation()})
	if !send("hello", hello) {
		return
	}
	for _, e := range sub.Snapshot {
		if !send("", e.JSON) {
			return
		}
	}
	for {
		e, err := sub.Next(r.Context())
		switch {
		case err == nil:
			if !send("", e.JSON) {
				return
			}
		case errors.Is(err, io.EOF):
			end, _ := json.Marshal(map[string]int{"exit": sub.Exit()})
			send("end", end)
			return
		default: // 切断・遅い購読者として外された: 何も足さずに閉じる (読み直すなら、繋ぎ直す)
			return
		}
	}
}

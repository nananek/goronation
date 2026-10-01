//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// chatMaxBody は、書き込み API の本文の上限 (バイト)。text の上限 (chat.MaxMessageBytes) の、JSON のエスケープ (最大 6 倍) 込みの分。
// form の回答 (フィールドの数 × 回答 1 つの上限 (4,000 字) × エスケープ 6 倍 + 余白) も、この上限に収まる (chat_http_test.go が固定する)。
const chatMaxBody = 6*chat.MaxMessageBytes + 1024

// chatMaxSSE は、同時に繋がる SSE の数の上限 (超過は 503)。web が持つ上限とは別の、この serve の最後の砦。
const chatMaxSSE = 32

// chatSSEWriteTimeout は、SSE の 1 回の書き込みに許す時間。読まない相手が、書き込みを、無期限に止めない。
const chatSSEWriteTimeout = 30 * time.Second

// chatHandler は、goronation serve --chat の UDS の HTTP API (認証なし。UDS に繋がれること自体が信頼境界):
//
//	GET  /events      SSE。event: hello (first_seq・generation・resumed・durable) → (after・generation が正しければ、耐久ログの続き) → バッファ → ライブ (id: <seq>・data: は Event の JSON 1 行) → event: end (exit)。クエリ after・generation (ADR 0024・0054)
//	POST /message     {"text"}
//	POST /permission  {"generation","request_id","outcome","content_hash"?}  (generation は必須。欠落 400・別の起動 409。content_hash は、見た要求の値の写し)
//	POST /form        {"generation","request_id","outcome":"answered"|"cancelled","answer"?:{"<key>":"<文字列>"|["<文字列>",…]},"content_hash"?}  (ADR 0046)
//	POST /stop        {} (本文なし可)
//
// 書き込みの本文は、上限つき・厳格な JSON (未知のフィールド・後ろの余分な値・重複したキーは 400)。エラーは {"error":"<コード>"}。
// 世代なしの承認の経路は無い (chat.Conversation の resolve は非公開)。
type chatHandler struct {
	s           *chatSession
	sse         atomic.Int32
	backfillMax int // 1 つの接続で、Backfill から送るバイト数の上限
}

func newChatHandler(s *chatSession) http.Handler { return newChatHandlerWith(s, chatBackfillMaxBytes) }

func newChatHandlerWith(s *chatSession, backfillMax int) http.Handler {
	h := &chatHandler{s: s, backfillMax: backfillMax}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", h.events)
	mux.HandleFunc("POST /message", h.message)
	mux.HandleFunc("POST /permission", h.permission)
	mux.HandleFunc("POST /form", h.form)
	mux.HandleFunc("POST /stop", h.stop)
	mux.HandleFunc("GET /version", handleVersion)
	return mux
}

// isJSONContentType は、Content-Type が application/json か (parameter・大文字小文字は許す)。web (web_chat.go) の関門も、同じ判定を使う。
func isJSONContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct)) == "application/json"
}

// decodeStrict は、本文 (Content-Type: application/json・上限つき) を、v に厳格に読む。本文なし (allowEmpty のとき) は通す。
func decodeStrict(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" || !allowEmpty {
		if !isJSONContentType(ct) {
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
	if hasDuplicateKeys(body) { // 後の値が勝つ (検証と実行の食い違いを、作らない)
		writeError(w, http.StatusBadRequest, "bad_json")
		return false
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

// hasDuplicateKeys は、JSON の中に、同じキーを 2 つ持つオブジェクトがあるか。最上位のキーは、Go の読み方 (構造体のフィールド名との照合は、大文字小文字を区別しない) に合わせて、
// encoding/json と同じ simple fold (U+017F ſ は s、U+212A K は k) で同一視する (読めない JSON は false。そのあとの Decode が落とす)。
func hasDuplicateKeys(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	return scanDuplicateKeys(dec, true)
}

// foldKey は、キーを、unicode.SimpleFold の同値類の最小の文字に寄せる (encoding/json が、構造体のフィールド名との照合に使う同一視と同じ)。
func foldKey(k string) string {
	return strings.Map(func(r rune) rune {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		return m
	}, k)
}

func scanDuplicateKeys(dec *json.Decoder, top bool) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return false
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return false
			}
			k, _ := kt.(string)
			if top {
				k = foldKey(k)
			}
			if seen[k] {
				return true
			}
			seen[k] = true
			if scanDuplicateKeys(dec, false) {
				return true
			}
		}
		dec.Token() // '}'
	case '[':
		for dec.More() {
			if scanDuplicateKeys(dec, false) {
				return true
			}
		}
		dec.Token() // ']'
	}
	return false
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
		// ContentHash は、利用者が見た要求の content_hash の写し (ADR 0042)。保持した値と違えば 409 content_changed。
		ContentHash string `json:"content_hash"`
	}
	if !decodeStrict(w, r, &req, false) {
		return
	}
	if req.Generation == "" || req.RequestID == "" || req.Outcome == "" {
		writeError(w, http.StatusBadRequest, "generation_request_id_outcome_required")
		return
	}
	chatResult(w, h.s.Chat.Conv.ResolvePermissionIn(req.Generation, chat.PermissionResolve{RequestID: req.RequestID, Outcome: req.Outcome, ContentHash: req.ContentHash}))
}

// form は、form (AskUserQuestion など) への回答。回答は、保持した要求のフィールドに対してだけ検査され (不正は 400 bad_answer)、content_hash は保持した値と照合される。
func (h *chatHandler) form(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Generation string `json:"generation"`
		chat.FormResolve
	}
	if !decodeStrict(w, r, &req, false) {
		return
	}
	if req.Generation == "" || req.RequestID == "" || req.Outcome == "" {
		writeError(w, http.StatusBadRequest, "generation_request_id_outcome_required")
		return
	}
	chatResult(w, h.s.Chat.Conv.ResolveFormIn(req.Generation, req.RequestID, req.FormResolve))
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
	case errors.Is(err, chat.ErrBadAnswer):
		writeError(w, http.StatusBadRequest, "bad_answer")
	case errors.Is(err, chat.ErrContentHashRequired):
		writeError(w, http.StatusBadRequest, "content_hash_required")
	case errors.Is(err, chat.ErrContentChanged):
		writeError(w, http.StatusConflict, "content_changed")
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

// chatMaxAfter は、after の上限 (JS の Number.isSafeInteger の範囲: 2^53 - 1)。
const chatMaxAfter = 1<<53 - 1

// chatBackfillMaxBytes は、1 つの接続で、耐久ログ (Backfill) から送るバイト数 (Event の JSON の合計) の上限。超えたら閉じる: 画面は、進んだ after で繋ぎ直す
// (1 回ごとに前進する。ADR 0024 決定 8・ADR 0054)。
const chatBackfillMaxBytes = 16 << 20

// resumeAfter は、クエリの after・generation が、どちらも正しいときだけ、after を返す (ADR 0024 決定 1): after は 10 進数の非負整数 (16 桁まで・2^53 未満)・
// generation は、現在の世代と完全に一致・どちらもキーが 1 つだけ。そうでなければ、エラーにせず、全再送 (Subscribe) に倒す。Last-Event-ID は、見ない (ADR 0054)。
func resumeAfter(q url.Values, generation string) (uint64, bool) {
	a, g := q["after"], q["generation"]
	if len(a) != 1 || len(g) != 1 || g[0] != generation || len(a[0]) == 0 || len(a[0]) > 16 {
		return 0, false
	}
	for i := 0; i < len(a[0]); i++ {
		if a[0][i] < '0' || a[0][i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(a[0], 10, 64)
	if err != nil || n > chatMaxAfter {
		return 0, false
	}
	return n, true
}

// subscribe は、events の購読を始める: after・generation が正しければ SubscribeAfter (ErrBadAfter は全再送に倒す)・そうでなければ Subscribe。
func (h *chatHandler) subscribe(r *http.Request) (*chat.Subscription, error) {
	hub := h.s.Chat.Hub
	if after, ok := resumeAfter(r.URL.Query(), h.s.Chat.Conv.Generation()); ok {
		sub, err := hub.SubscribeAfter(after)
		if !errors.Is(err, chat.ErrBadAfter) {
			return sub, err
		}
	}
	return hub.Subscribe()
}

// events は、SSE。data: は、Event の JSON (改行を含まない) の 1 行。Event には、id: <seq> の行が付く (hello・end には付けない)。送る順は、hello → Backfill (耐久ログ。after が効いたときだけ)
// → Snapshot → ライブ。Backfill の失敗・上限は、何も足さずに閉じる (error の文字列を、出さない)。
func (h *chatHandler) events(w http.ResponseWriter, r *http.Request) {
	if h.sse.Add(1) > chatMaxSSE {
		h.sse.Add(-1)
		writeError(w, http.StatusServiceUnavailable, "too_many_streams")
		return
	}
	defer h.sse.Add(-1)
	sub, err := h.subscribe(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "too_many_subscribers")
		return
	}
	defer sub.Close()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, seq *uint64, data []byte) bool {
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
		if seq != nil {
			buf.WriteString("id: ")
			buf.WriteString(strconv.FormatUint(*seq, 10))
			buf.WriteByte('\n')
		}
		buf.WriteString("data: ")
		buf.Write(data)
		buf.WriteString("\n\n")
		if _, err := w.Write(buf.Bytes()); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	sendEvent := func(e chat.Event) bool { return send("", &e.Seq, e.JSON) }
	hello, _ := json.Marshal(map[string]any{
		"first_seq": sub.FirstSeq, "generation": h.s.Chat.Conv.Generation(),
		"resumed": sub.Resumed(), "durable": sub.Durable(),
	})
	if !send("hello", nil, hello) {
		return
	}
	if sub.Backfill != nil {
		sent := 0
		for {
			evs, err := sub.Backfill.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil || r.Context().Err() != nil { // 古い・読めない・不正な行: 何も足さずに閉じる (画面は、繋ぎ直す)
				return
			}
			for _, e := range evs {
				if sent += len(e.JSON); sent > h.backfillMax {
					return
				}
				if !sendEvent(e) {
					return
				}
			}
		}
	}
	for _, e := range sub.Snapshot {
		if !sendEvent(e) {
			return
		}
	}
	for {
		e, err := sub.Next(r.Context())
		switch {
		case err == nil:
			if !sendEvent(e) {
				return
			}
		case errors.Is(err, io.EOF):
			end, _ := json.Marshal(map[string]int{"exit": sub.Exit()})
			send("end", nil, end)
			return
		default: // 切断・遅い購読者として外された: 何も足さずに閉じる (読み直すなら、繋ぎ直す)
			return
		}
	}
}

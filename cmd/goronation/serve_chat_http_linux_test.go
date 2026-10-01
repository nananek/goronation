package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// newInProcChat は、檻を使わずに、偽の claude (runFakeChat) をプロセス内で動かす chatSession (HTTP API の確認用)。
func newInProcChat(t *testing.T, mode string) (*chatSession, *httptest.Server) {
	t.Helper()
	s := newInProcChatSession(t, mode)
	srv := httptest.NewServer(newChatHandler(s))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return s, srv
}

// newInProcChatSession は、newInProcChat の、HTTP を付けない形 (web の中継のテストが、UDS に載せる)。
func newInProcChatSession(t *testing.T, mode string) *chatSession {
	t.Helper()
	toAgentR, toAgentW := io.Pipe()
	fromAgentR, fromAgentW := io.Pipe()
	launch := mustLaunch(t)
	s := &chatSession{id: "sess", done: make(chan struct{})}
	s.Chat = chat.NewSession(chat.SessionConfig{
		Launch: launch, ID: "sess", Input: toAgentW,
		OnStop: func() { toAgentW.Close() },
	})
	go func() {
		runFakeChat(mode, toAgentR, fromAgentW)
		fromAgentW.Close()
	}()
	go func() {
		s.Chat.ReadOutput(fromAgentR)
		s.Chat.Finish(0)
	}()
	t.Cleanup(s.Chat.Conv.Stop)
	return s
}

// sse は、GET /events を読む: 行ごとに、event 名と data を返す。
type sseFrame struct{ event, id, data string }

type sseReader struct {
	t     *testing.T
	body  io.ReadCloser
	sc    *bufio.Scanner
	raw   []string // 届いた行 (区切りの空行を含む)
	pings int      // 届いた心拍 (: ping) の数 (web が足す)
}

func openSSE(t *testing.T, srv *httptest.Server) *sseReader {
	t.Helper()
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	t.Cleanup(func() { resp.Body.Close() })
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 4<<20)
	return &sseReader{t: t, body: resp.Body, sc: sc}
}

// next は、次のイベント (event 行が無ければ event は空) を返す。ストリームが終われば ok=false。フレーミングの規則 (行は event:・data:・空行だけ・
// data は 1 行) が破れたら、失敗にする。
func (r *sseReader) next() (f sseFrame, ok bool) {
	r.t.Helper()
	dataLines, ping := 0, false
	for r.sc.Scan() {
		line := r.sc.Text()
		r.raw = append(r.raw, line)
		switch {
		case line == ": ping":
			r.pings++
			ping = true
		case line == "" && dataLines == 0 && ping: // 心拍の後の空行
			ping = false
		case line == "":
			if dataLines != 1 {
				r.t.Fatalf("data 行が %d 本のイベント (1 本のはず): %q", dataLines, r.raw)
			}
			return f, true
		case strings.HasPrefix(line, "id: "): // 10 進数だけ・1 イベントに 1 行 (ADR 0054)
			if f.id != "" || !chatIDLineRE.MatchString(line) {
				r.t.Fatalf("id 行が不正: %q (全体 %q)", line, r.raw)
			}
			f.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.data = strings.TrimPrefix(line, "data: ")
			dataLines++
		default:
			r.t.Fatalf("SSE のフレーミングが破れた行: %q (全体 %q)", line, r.raw)
		}
	}
	return f, false
}

// until は、pred に合うイベントまで読んで返す。
func (r *sseReader) until(pred func(sseFrame) bool) sseFrame {
	r.t.Helper()
	for {
		f, ok := r.next()
		if !ok {
			r.t.Fatalf("目的のイベントの前に、ストリームが終わった: %q", r.raw)
		}
		if pred(f) {
			return f
		}
	}
}

func evType(typ string) func(sseFrame) bool {
	return func(f sseFrame) bool {
		var v struct {
			Type string `json:"type"`
		}
		return f.event == "" && json.Unmarshal([]byte(f.data), &v) == nil && v.Type == typ
	}
}

func post(t *testing.T, srv *httptest.Server, path, ctype, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	return post(t, srv, path, "application/json", body)
}

// permissionEvent は、permission.requested を読み、request_id と content_hash を返す (応答に写す。ADR 0042 決定 3: 必須)。
func permissionEvent(t *testing.T, sse *sseReader) (id, hash string) {
	t.Helper()
	f := sse.until(evType("permission.requested"))
	var v struct {
		Data struct {
			RequestID   string `json:"request_id"`
			ContentHash string `json:"content_hash"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.data), &v); err != nil || v.Data.RequestID == "" || !strings.HasPrefix(v.Data.ContentHash, "sha256:") {
		t.Fatalf("permission.requested = %s", f.data)
	}
	return v.Data.RequestID, v.Data.ContentHash
}

// postPermission は、受けた content_hash を写して POST /permission する。
func postPermission(t *testing.T, srv *httptest.Server, gen, id, hash, outcome string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"generation": gen, "request_id": id, "outcome": outcome, "content_hash": hash})
	return postJSON(t, srv, "/permission", string(b))
}

// postForm は、受けた content_hash を写して POST /form する (answer は、answered のときだけ)。
func postForm(t *testing.T, srv *httptest.Server, gen, id, hash, outcome string, answer map[string]any) (int, string) {
	t.Helper()
	m := map[string]any{"generation": gen, "request_id": id, "outcome": outcome, "content_hash": hash}
	if answer != nil {
		m["answer"] = answer
	}
	b, _ := json.Marshal(m)
	return postJSON(t, srv, "/form", string(b))
}

func withTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	timer := time.AfterFunc(d, func() { panic("timeout: " + t.Name()) })
	t.Cleanup(func() { timer.Stop() })
}

// hello (世代・first_seq) → 指示 → 権限要求 → 世代の検査 (欠落 400・別の起動 409・正しければ通る・二重は 409) → 完了 → 手動の終了で end。
func TestChatHTTPFlow(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, srv := newInProcChat(t, "flow")
	sse := openSSE(t, srv)

	hello, _ := sse.next()
	var h struct {
		FirstSeq   *uint64 `json:"first_seq"`
		Generation string  `json:"generation"`
	}
	if hello.event != "hello" || json.Unmarshal([]byte(hello.data), &h) != nil || h.FirstSeq == nil || h.Generation != s.Chat.Conv.Generation() || len(h.Generation) != 32 {
		t.Fatalf("hello = %+v", hello)
	}

	if code, body := postJSON(t, srv, "/message", `{"text":"hi"}`); code != 200 {
		t.Fatalf("message: %d %s", code, body)
	}
	if code, _ := postJSON(t, srv, "/message", `{"text":"again"}`); code != 409 { // ターンの途中
		t.Errorf("ターン中の message = %d (409 のはず)", code)
	}
	id, hash := permissionEvent(t, sse)
	body := func(gen, outcome string) string {
		b, _ := json.Marshal(map[string]string{"generation": gen, "request_id": id, "outcome": outcome, "content_hash": hash})
		return string(b)
	}
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"世代なし", `{"request_id":"` + id + `","outcome":"allow_once","content_hash":"` + hash + `"}`, 400},
		{"世代が空", body("", "allow_once"), 400},
		{"別の起動の世代", body("00000000000000000000000000000000", "allow_once"), 409},
		{"未知の request_id", `{"generation":"` + h.Generation + `","request_id":"nope","outcome":"allow_once","content_hash":"` + hash + `"}`, 404},
		{"不正な outcome", body(h.Generation, "allow_always"), 400},
	} {
		if code, b := postJSON(t, srv, "/permission", tc.body); code != tc.want {
			t.Errorf("%s: %d %s (%d のはず)", tc.name, code, b, tc.want)
		}
	}
	if s.Chat.Conv.Pending() != 1 {
		t.Fatalf("失敗した承認で、未決が動いた: pending=%d", s.Chat.Conv.Pending())
	}
	if code, b := postJSON(t, srv, "/permission", body(h.Generation, "allow_once")); code != 200 {
		t.Fatalf("承認: %d %s", code, b)
	}
	if code, _ := postJSON(t, srv, "/permission", body(h.Generation, "allow_once")); code != 409 {
		t.Errorf("二重の承認 = %d (409 のはず)", code)
	}
	sse.until(evType("turn.completed"))

	if code, _ := postJSON(t, srv, "/stop", ``); code != 200 {
		t.Errorf("stop = %d", code)
	}
	end := sse.until(func(f sseFrame) bool { return f.event == "end" })
	if end.data != `{"exit":0}` {
		t.Errorf("end = %q", end.data)
	}
	if _, ok := sse.next(); ok {
		t.Error("end の後も、ストリームが続いた")
	}
	if code, _ := postJSON(t, srv, "/message", `{"text":"late"}`); code != 409 {
		t.Errorf("終了後の message = %d (409 のはず)", code)
	}
}

// 世代は、hello から得た値で、別の起動 (別の Conversation) の値では通らない (L12: 起動をまたぐ承認の誤適用)。
func TestChatHTTPGenerationDiffersPerLaunch(t *testing.T) {
	withTimeout(t, 30*time.Second)
	_, srv1 := newInProcChat(t, "hold")
	_, srv2 := newInProcChat(t, "hold")
	gen := func(srv *httptest.Server) string {
		hello, _ := openSSE(t, srv).next()
		var h struct{ Generation string }
		json.Unmarshal([]byte(hello.data), &h)
		return h.Generation
	}
	g1, g2 := gen(srv1), gen(srv2)
	if g1 == "" || g1 == g2 {
		t.Fatalf("世代が起動ごとに違わない: %q %q", g1, g2)
	}
	if code, _ := postJSON(t, srv2, "/permission", `{"generation":"`+g1+`","request_id":"r","outcome":"allow_once","content_hash":"sha256:00"}`); code != 409 {
		t.Errorf("別の起動の世代 = %d (409 のはず)", code)
	}
}

// 書き込み API の本文は、厳格な JSON・上限つき。
func TestChatHTTPStrictBodies(t *testing.T) {
	withTimeout(t, 30*time.Second)
	_, srv := newInProcChat(t, "hold")
	tooBig := `{"text":"` + strings.Repeat("a", chatMaxBody) + `"}`
	for _, tc := range []struct {
		name, path, ctype, body string
		want                    int
	}{
		{"未知のフィールド", "/message", "application/json", `{"text":"x","extra":1}`, 400},
		{"後ろに別の値", "/message", "application/json", `{"text":"x"}{"text":"y"}`, 400},
		{"後ろのゴミ", "/message", "application/json", `{"text":"x"} junk`, 400},
		{"JSON でない", "/message", "application/json", `text=x`, 400},
		{"text が無い", "/message", "application/json", `{}`, 400},
		{"text が空", "/message", "application/json", `{"text":""}`, 400},
		{"text が数値", "/message", "application/json", `{"text":1}`, 400},
		{"text が大きい", "/message", "application/json", `{"text":"` + strings.Repeat("a", chat.MaxMessageBytes+1) + `"}`, 413},
		{"本文が大きい", "/message", "application/json", tooBig, 413},
		{"Content-Type が違う", "/message", "text/plain", `{"text":"x"}`, 415},
		{"Content-Type が無い", "/message", "", `{"text":"x"}`, 415},
		{"permission の未知のフィールド", "/permission", "application/json", `{"generation":"g","request_id":"r","outcome":"allow_once","x":1}`, 400},
		{"permission の空の本文", "/permission", "application/json", ``, 400},
		{"stop の未知のフィールド", "/stop", "application/json", `{"x":1}`, 400},
		{"stop の Content-Type が違う", "/stop", "text/plain", `{}`, 415},
	} {
		if code, b := post(t, srv, tc.path, tc.ctype, tc.body); code != tc.want {
			t.Errorf("%s: %d %.100s (%d のはず)", tc.name, code, b, tc.want)
		}
	}
	// 通る形 (Content-Type の parameter・大文字小文字)。
	if code, b := post(t, srv, "/message", "Application/JSON; charset=utf-8", `{"text":"ok"}`); code != 200 {
		t.Errorf("正しい message = %d %s", code, b)
	}
	// method が違うものは、405。
	for _, path := range []string{"/message", "/permission", "/stop"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d (405 のはず)", path, resp.StatusCode)
		}
	}
	resp, _ := http.Post(srv.URL+"/events", "application/json", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /events = %d", resp.StatusCode)
	}
}

// 改行・CR・U+2028 を含む敵対テキストは、JSON のエスケープで 1 行のまま、別のイベント (end・hello) を偽造できない。
func TestChatHTTPSSECannotBeForged(t *testing.T) {
	withTimeout(t, 30*time.Second)
	_, srv := newInProcChat(t, "hold")
	sse := openSSE(t, srv)
	sse.next() // hello

	evil := "a\n\nevent: end\ndata: {\"exit\":0}\n\r\r\ndata: x\u2028y\u2029z\r\nevent: hello\n\n"
	b, _ := json.Marshal(map[string]string{"text": evil})
	if code, body := postJSON(t, srv, "/message", string(b)); code != 200 {
		t.Fatalf("message: %d %s", code, body)
	}
	f := sse.until(evType("turn.started")) // next() が、フレーミングの破れ (data 行が 1 本でない・知らない行) を、失敗にする
	var v struct {
		Data struct{ Text string } `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.data), &v); err != nil || v.Data.Text != evil {
		t.Fatalf("送った text が、そのまま戻らない: %q (%v)", v.Data.Text, err)
	}
	if strings.ContainsAny(f.data, "\r\n") {
		t.Errorf("data に、生の改行がある: %q", f.data)
	}
	hellos := 0
	for _, l := range sse.raw {
		if l == "event: end" {
			t.Errorf("偽造された end: %q", sse.raw)
		}
		if l == "event: hello" {
			hellos++
		}
	}
	if hellos != 1 {
		t.Errorf("hello が %d 個 (1 個のはず): %q", hellos, sse.raw)
	}
}

// Hub が (バグで) 生の改行を含む JSON を持っても、serve は、その Event を書かずに捨てる: send の改行の検査が、json.Marshal の二重防御の、もう一方。
func TestChatHTTPSSEDropsRawNewlineEvent(t *testing.T) {
	withTimeout(t, 30*time.Second)
	s, srv := newInProcChat(t, "hold")
	sse := openSSE(t, srv)
	sse.next() // hello

	for i, bad := range []string{"{\"type\":\"x\"}\n\nevent: end\ndata: {\"exit\":0}", "{\"type\":\"x\"}\r\r\ndata: y"} {
		s.Chat.Hub.Publish(chat.Event{Seq: 1000 + uint64(i)*2, Type: "x", JSON: []byte(bad)})
		s.Chat.Hub.Publish(chat.Event{Seq: 1001 + uint64(i)*2, Type: "ok", JSON: []byte(`{"type":"ok"}`)})
		f := sse.until(evType("ok")) // 悪い Event の後の、正しい Event は届く (接続は閉じない)
		if want := strconv.Itoa(1001 + i*2); f.id != want {
			t.Errorf("id = %q, want %q", f.id, want)
		}
	}
	for _, l := range sse.raw {
		if l == "event: end" || strings.Contains(l, "\r") {
			t.Errorf("偽造された行が届いた: %q", sse.raw)
		}
	}
}

// 本番の既定は、content_hash 必須 (ADR 0042 決定 3): 無ければ 400 content_hash_required・違えば 409 content_changed・写せば 200。拒否では未決が動かない。
// 限界: 内容の取り違え (別タブ・古い画面) を防ぐもので、乗っ取り (hash を読める者が、その値を写す) には効かない (ADR 0042 決定 5)。
func TestChatHTTPContentHashRequired(t *testing.T) {
	withTimeout(t, 60*time.Second)
	for _, kind := range []string{"permission", "form"} {
		t.Run(kind, func(t *testing.T) {
			mode, msg := "flow", "hi"
			if kind == "form" {
				mode, msg = "ask", "ask"
			}
			s, srv := newInProcChat(t, mode)
			sse := openSSE(t, srv)
			hello, _ := sse.next()
			var h struct{ Generation string }
			json.Unmarshal([]byte(hello.data), &h)
			if code, b := postJSON(t, srv, "/message", `{"text":"`+msg+`"}`); code != 200 {
				t.Fatalf("message: %d %s", code, b)
			}
			var id, hash string
			resolve := func(hash string) (int, string) {
				m := map[string]any{"generation": h.Generation, "request_id": id}
				if hash != "" {
					m["content_hash"] = hash
				}
				if kind == "form" {
					m["outcome"], m["answer"] = "answered", map[string]any{"q0": "青"}
				} else {
					m["outcome"] = "allow_once"
				}
				b, _ := json.Marshal(m)
				return postJSON(t, srv, "/"+kind, string(b))
			}
			if kind == "form" {
				id, hash = formEvent(t, sse)
			} else {
				id, hash = permissionEvent(t, sse)
			}
			if code, b := resolve(""); code != 400 || errCode(b) != "content_hash_required" {
				t.Errorf("hash 無し: %d %s", code, b)
			}
			if code, b := resolve("sha256:00"); code != 409 || errCode(b) != "content_changed" {
				t.Errorf("違う hash: %d %s", code, b)
			}
			if s.Chat.Conv.Pending() != 1 {
				t.Fatalf("拒否された応答で、未決が動いた: pending=%d", s.Chat.Conv.Pending())
			}
			if code, b := resolve(hash); code != 200 {
				t.Errorf("正しい hash: %d %s", code, b)
			}
		})
	}
}

// 再接続 (2 本目の SSE) は、hello から、バッファの全部を受け取り、そのあとライブ。
func TestChatHTTPReconnectReplaysBuffer(t *testing.T) {
	withTimeout(t, 30*time.Second)
	_, srv := newInProcChat(t, "hold")
	sse1 := openSSE(t, srv)
	sse1.next()
	postJSON(t, srv, "/message", `{"text":"first"}`)
	sse1.until(evType("turn.started"))

	sse2 := openSSE(t, srv)
	h, _ := sse2.next()
	if h.event != "hello" {
		t.Fatalf("hello = %+v", h)
	}
	sse2.until(evType("turn.started"))
}

// 切断された購読は、Hub から外れる。同時接続の上限を超えたら、503。
func TestChatHTTPSSELimit(t *testing.T) {
	withTimeout(t, 60*time.Second)
	_, srv := newInProcChat(t, "hold")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var bodies []io.Closer
	for i := 0; i < chatMaxSSE; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%d 本目: %v %v", i, resp, err)
		}
		bodies = append(bodies, resp.Body)
	}
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("上限を超えた SSE = %d (503 のはず)", resp.StatusCode)
	}
	bodies[0].Close() // 1 本切ると、また繋がる
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(srv.URL + "/events")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if err == nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("切断した枠が、戻らない")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 書き込みの本文の上限 (chatMaxBody) は、form の回答 (フィールド数 × 回答 1 つの上限 × エスケープの 6 倍) を収める。
func TestChatMaxBodyFitsFormAnswers(t *testing.T) {
	if need := 6*chat.MaxFormFields*chat.MaxFormAnswerText + 1024; chatMaxBody < need {
		t.Fatalf("chatMaxBody = %d < %d (form の回答が収まらない)", chatMaxBody, need)
	}
}

func TestHasDuplicateKeys(t *testing.T) {
	for body, want := range map[string]bool{
		`{"a":1,"b":2}`:                                  false,
		`{"a":1,"a":2}`:                                  true,
		`{"generation":"x","Generation":"y"}`:            true, // 構造体への読みは、大文字小文字を区別しない (最上位だけ)
		`{"answer":{"q0":"a","q0":"b"}}`:                 true,
		`{"answer":{"q0":"a","Q0":"b"}}`:                 false, // 回答のキーは、区別する
		`{"a":[{"k":1,"k":2}]}`:                          true,
		`{"a":[{"k":1},{"k":2}]}`:                        false,
		`{"a":{"b":1},"c":{"b":2}}`:                      false,
		"{\"answer\":{},\"anſwer\":{}}":                  true, // U+017F (ſ) は、Go の構造体への読みで s と同一視される
		"{\"content_hash\":\"x\",\"content_haſh\":\"\"}": true,
		`not json`: false,
		``:         false,
	} {
		if got := hasDuplicateKeys([]byte(body)); got != want {
			t.Errorf("%q = %v, want %v", body, got, want)
		}
	}
}

// formEvent は、SSE から、form.requested の (request_id・content_hash) を取る。
func formEvent(t *testing.T, sse *sseReader) (id, hash string) {
	t.Helper()
	f := sse.until(evType("form.requested"))
	var v struct {
		Data struct {
			RequestID   string `json:"request_id"`
			ContentHash string `json:"content_hash"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.data), &v); err != nil || v.Data.RequestID == "" || !strings.HasPrefix(v.Data.ContentHash, "sha256:") {
		t.Fatalf("form.requested = %s", f.data)
	}
	return v.Data.RequestID, v.Data.ContentHash
}

func errCode(body string) string {
	var v struct {
		Error string `json:"error"`
	}
	json.Unmarshal([]byte(body), &v)
	return v.Error
}

// AskUserQuestion: form.requested (content_hash つき・permission.requested でない) → POST /form (検査の拒否は何も動かさない) → 回答が claude に届く (updatedInput.answers)。
func TestChatHTTPFormFlow(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, srv := newInProcChat(t, "ask")
	sse := openSSE(t, srv)
	hello, _ := sse.next()
	var h struct{ Generation string }
	json.Unmarshal([]byte(hello.data), &h)
	if code, b := postJSON(t, srv, "/message", `{"text":"ask me"}`); code != 200 {
		t.Fatalf("message: %d %s", code, b)
	}
	id, hash := formEvent(t, sse)
	if s.Chat.Conv.Pending() != 1 {
		t.Fatalf("pending = %d", s.Chat.Conv.Pending())
	}
	body := func(extra string) string {
		return `{"generation":"` + h.Generation + `","request_id":"` + id + `",` + extra + `}`
	}
	good := `"outcome":"answered","answer":{"q0":"青"},"content_hash":"` + hash + `"`
	for _, tc := range []struct {
		name, path, ctype, body string
		code                    int
		errc                    string
	}{
		{"世代なし", "/form", "application/json", `{"request_id":"` + id + `","outcome":"cancelled"}`, 400, "generation_request_id_outcome_required"},
		{"別の起動の世代", "/form", "application/json", `{"generation":"00000000000000000000000000000000","request_id":"` + id + `","outcome":"cancelled"}`, 409, "stale_generation"},
		{"未知の request_id", "/form", "application/json", `{"generation":"` + h.Generation + `","request_id":"nope","outcome":"cancelled"}`, 404, "unknown_request"},
		{"保持した form に無いキー", "/form", "application/json", body(`"outcome":"answered","answer":{"q9":"x"},"content_hash":"` + hash + `"`), 400, "bad_answer"},
		{"回答が配列 (単一の質問)", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":["青"]},"content_hash":"` + hash + `"`), 400, "bad_answer"},
		{"回答が数値", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":1}`), 400, "bad_json"},
		{"回答が null", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":null}`), 400, "bad_json"},
		{"配列の要素が数値", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":[1]}`), 400, "bad_json"},
		{"cancelled に answer", "/form", "application/json", body(`"outcome":"cancelled","answer":{"q0":"青"},"content_hash":"` + hash + `"`), 400, "bad_answer"},
		{"知らない outcome", "/form", "application/json", body(`"outcome":"allow_once"`), 400, "bad_request"},
		{"content_hash が違う", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":"青"},"content_hash":"sha256:00"`), 409, "content_changed"},
		{"重複したキー (answer)", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":"赤","q0":"青"}`), 400, "bad_json"},
		{"重複したキー (最上位・大文字小文字違い)", "/form", "application/json", body(`"outcome":"cancelled","Outcome":"answered"`), 400, "bad_json"},
		{"未知のフィールド", "/form", "application/json", body(`"outcome":"cancelled","x":1`), 400, "bad_json"},
		{"空の本文", "/form", "application/json", ``, 400, "bad_json"},
		{"Content-Type が違う", "/form", "text/plain", body(good), 415, "content_type"},
		{"本文が大きい", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":"` + strings.Repeat("a", chatMaxBody) + `"}`), 413, "too_large"},
		{"長い回答 (本文は上限内)", "/form", "application/json", body(`"outcome":"answered","answer":{"q0":"` + strings.Repeat("あ", chat.MaxFormAnswerText+1) + `"},"content_hash":"` + hash + `"`), 400, "bad_answer"},
		{"form に permission の応答", "/permission", "application/json", body(`"outcome":"allow_once"`), 404, "unknown_request"},
	} {
		code, b := post(t, srv, tc.path, tc.ctype, tc.body)
		if code != tc.code || errCode(b) != tc.errc {
			t.Errorf("%s: %d %.100s (%d %s のはず)", tc.name, code, b, tc.code, tc.errc)
		}
	}
	if s.Chat.Conv.Pending() != 1 {
		t.Fatalf("拒否された応答で、未決が動いた: pending=%d", s.Chat.Conv.Pending())
	}
	if code, b := postJSON(t, srv, "/form", body(good)); code != 200 {
		t.Fatalf("回答: %d %s", code, b)
	}
	if code, b := postJSON(t, srv, "/form", body(good)); code != 409 || errCode(b) != "already_resolved" {
		t.Errorf("二重の回答: %d %s", code, b)
	}
	resolved := sse.until(evType("form.resolved"))
	if !strings.Contains(resolved.data, `"by":"human"`) || !strings.Contains(resolved.data, `"outcome":"answered"`) {
		t.Errorf("form.resolved = %s", resolved.data)
	}
	// 回答が claude に届いた: 保持した質問の文をキーにした answers。
	msg := sse.until(func(f sseFrame) bool { return evType("message.text")(f) && strings.Contains(f.data, "request=") })
	if !strings.Contains(msg.data, `answers`) || !strings.Contains(msg.data, `好きな色は?`) || !strings.Contains(msg.data, `青`) || !strings.Contains(msg.data, `allow`) {
		t.Fatalf("claude が受けた応答: %s", msg.data)
	}
	sse.until(evType("turn.completed"))
}

// 取り消し (cancelled) は deny。permission の応答 (content_hash つき) も、保持した値と照合する。
func TestChatHTTPFormCancelAndPermissionHash(t *testing.T) {
	withTimeout(t, 60*time.Second)
	_, srv := newInProcChat(t, "ask")
	sse := openSSE(t, srv)
	hello, _ := sse.next()
	var h struct{ Generation string }
	json.Unmarshal([]byte(hello.data), &h)
	postJSON(t, srv, "/message", `{"text":"ask"}`)
	id, fhash := formEvent(t, sse)
	if code, b := postForm(t, srv, h.Generation, id, fhash, "cancelled", nil); code != 200 {
		t.Fatalf("取り消し: %d %s", code, b)
	}
	msg := sse.until(func(f sseFrame) bool { return evType("message.text")(f) && strings.Contains(f.data, "request=") })
	if !strings.Contains(msg.data, `deny`) {
		t.Fatalf("取り消しが deny で届かない: %s", msg.data)
	}

	_, srv2 := newInProcChat(t, "flow")
	sse2 := openSSE(t, srv2)
	hello, _ = sse2.next()
	json.Unmarshal([]byte(hello.data), &h)
	postJSON(t, srv2, "/message", `{"text":"hi"}`)
	req := sse2.until(evType("permission.requested"))
	var rv struct {
		Data struct {
			RequestID, ContentHash, Summary string
			Details                         []struct{ Label, Text string }
		} `json:"data"`
	}
	json.Unmarshal([]byte(req.data), &rv)
	// 汎用の JSON のキー名 (request_id) は、構造体のタグ無しでも大文字小文字を区別せず読める。
	var rv2 struct {
		Data struct {
			RequestID   string `json:"request_id"`
			ContentHash string `json:"content_hash"`
			Summary     string `json:"summary"`
			Details     []struct{ Label, Text string }
		} `json:"data"`
	}
	json.Unmarshal([]byte(req.data), &rv2)
	d := rv2.Data
	if !strings.HasPrefix(d.ContentHash, "sha256:") || d.Summary != "Write: /work/new.txt" || len(d.Details) != 2 {
		t.Fatalf("permission.requested = %s", req.data)
	}
	pbody := func(extra string) string {
		return `{"generation":"` + h.Generation + `","request_id":"` + d.RequestID + `","outcome":"allow_once"` + extra + `}`
	}
	if code, b := postJSON(t, srv2, "/permission", pbody(`,"content_hash":"sha256:00"`)); code != 409 || errCode(b) != "content_changed" {
		t.Errorf("違う hash: %d %s", code, b)
	}
	if code, b := postJSON(t, srv2, "/permission", pbody(`,"content_hash":"`+d.ContentHash+`"`)); code != 200 {
		t.Errorf("正しい hash: %d %s", code, b)
	}
}

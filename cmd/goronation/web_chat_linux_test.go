package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/session"
	iwebauthn "github.com/nananek/goronation/cmd/internal/webauthn"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// goronation web の、構造化チャットの中継 (web_chat.go) のテスト。web は、実際の httptest.Server (ログイン済みの cookie つき)、serve の側は、UDS の
// 上の、本物のハンドラ (newChatHandler。プロセス内の偽の claude) か、性質を決めた偽の上流 (止まる・読まれない・不正な行)。bwrap は要らない。

const (
	chatTestID  = "20260101-000000-abcdef"
	chatTestID2 = "20260101-000000-abcde0"
)

// countListener は、Accept した接続の数を数える (web が、serve に触れなかったことの確認)。
type countListener struct {
	net.Listener
	n atomic.Int32
}

func (l *countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

type chatWeb struct {
	t        *testing.T
	web      *httptest.Server
	client   *http.Client
	origin   string
	stateDir string
}

// fastRelay は、テストが待てる長さの上限・期限 (本番の値は defaultChatRelay)。
func fastRelay() chatRelayConfig {
	return chatRelayConfig{
		MaxSSE: 3, MaxSSEPerSession: 2,
		DialTimeout: 300 * time.Millisecond, HeaderTimeout: 400 * time.Millisecond, CallTimeout: 500 * time.Millisecond,
		SSEWriteTimeout: 300 * time.Millisecond, Heartbeat: 150 * time.Millisecond,
	}
}

// newChatWeb は、ログイン済みの web (relay の設定・http.Server の ReadTimeout・WriteTimeout つき) を起こす。状態のディレクトリは、UDS の path (107 バイト) に収まる短さ。
func newChatWeb(t *testing.T, relay chatRelayConfig, readTimeout, writeTimeout time.Duration) *chatWeb {
	t.Helper()
	stateDir := shortDir(t)
	store, err := iwebauthn.NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sessStore, err := session.NewStore(stateDir, bwrap.CurrentHost())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	srv.Config = newWebHTTPServerWith(srv.Config.Handler, readTimeout, writeTimeout)
	srv.Start()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	origin := srv.URL
	cfg := iwebauthn.Config{RPID: "127.0.0.1", RPName: "test", Origin: origin}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	handler = newWebMuxChat(cfg, store, origin, sessStore, "", stateDir, self, relay)
	return &chatWeb{t: t, web: srv, client: loggedInClient(t, srv, store, "127.0.0.1", origin), origin: origin, stateDir: stateDir}
}

// listen は、id の chat.sock で、h を待ち受ける (serve の代わり)。
func (c *chatWeb) listen(id string, h http.Handler) *countListener {
	c.t.Helper()
	sock := chatSocketPath(c.stateDir, defaultGroup, id)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		c.t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		c.t.Fatal(err)
	}
	cl := &countListener{Listener: l}
	srv := &http.Server{Handler: h}
	go srv.Serve(cl)
	c.t.Cleanup(func() { srv.Close() })
	return cl
}

// req は、web へ要求を送る。headers は、既定 (Origin と Content-Type: application/json) を上書きする (値が "" なら、そのヘッダを付けない)。
func (c *chatWeb) req(method, path, body string, headers map[string]string) (int, string) {
	c.t.Helper()
	return c.reqWith(c.client, method, path, body, headers)
}

func (c *chatWeb) reqWith(cli *http.Client, method, path, body string, headers map[string]string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.web.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	h := map[string]string{"Origin": c.origin, "Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	for k, v := range h {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := cli.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// events は、web の GET /s/{id}/events を開く。
func (c *chatWeb) events(id string) (*sseReader, int) {
	c.t.Helper()
	resp, err := c.client.Get(c.web.URL + "/s/" + id + "/events")
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 4<<20)
	return &sseReader{t: c.t, body: resp.Body, sc: sc}, resp.StatusCode
}

func (c *chatWeb) mustEvents(id string) *sseReader {
	c.t.Helper()
	r, code := c.events(id)
	if r == nil {
		c.t.Fatalf("events = %d", code)
	}
	return r
}

func evPath(id, op string) string { return "/s/" + id + "/" + op }

// 全体の流れ (web → serve の本物のハンドラ): hello の世代 → 指示 → 権限要求 → 世代の検査 (欠落 400・別の起動 409) → 承認 → 完了 → 終了で end。
func TestWebChatFlow(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, defaultChatRelay, webReadTimeout, webWriteTimeout)
	s := newInProcChatSession(t, "flow")
	cw.listen(chatTestID, newChatHandler(s))
	sse := cw.mustEvents(chatTestID)

	hello, _ := sse.next()
	var h struct {
		FirstSeq   *uint64 `json:"first_seq"`
		Generation string  `json:"generation"`
	}
	if hello.event != "hello" || json.Unmarshal([]byte(hello.data), &h) != nil || h.FirstSeq == nil || h.Generation != s.Chat.Conv.Generation() {
		t.Fatalf("hello = %+v", hello)
	}
	if code, b := cw.req("POST", evPath(chatTestID, "message"), `{"text":"hi"}`, nil); code != 200 {
		t.Fatalf("message: %d %s", code, b)
	}
	req := sse.until(evType("permission.requested"))
	var rv struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(req.data), &rv)
	perm := func(gen string) (int, string) {
		b, _ := json.Marshal(map[string]string{"generation": gen, "request_id": rv.Data.RequestID, "outcome": "allow_once"})
		return cw.req("POST", evPath(chatTestID, "permission"), string(b), nil)
	}
	if code, _ := cw.req("POST", evPath(chatTestID, "permission"), `{"request_id":"`+rv.Data.RequestID+`","outcome":"allow_once"}`, nil); code != 400 {
		t.Errorf("世代なし = %d (400 のはず)", code)
	}
	if code, _ := perm("00000000000000000000000000000000"); code != 409 {
		t.Errorf("別の起動の世代 = %d (409 のはず)", code)
	}
	if code, b := perm(h.Generation); code != 200 {
		t.Fatalf("承認: %d %s", code, b)
	}
	sse.until(evType("turn.completed"))
	if code, _ := cw.req("POST", evPath(chatTestID, "stop"), `{}`, nil); code != 200 {
		t.Errorf("stop = %d", code)
	}
	if end := sse.until(func(f sseFrame) bool { return f.event == "end" }); end.data != `{"exit":0}` {
		t.Errorf("end = %q", end.data)
	}
}

// 認証・関門・不正な要求は、serve に触れない (UDS への接続が 0 のまま)。
func TestWebChatGatesNeverReachServe(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	s := newInProcChatSession(t, "hold")
	l := cw.listen(chatTestID, newChatHandler(s))
	anon := &http.Client{}

	// 無認証は 401 (SSE も書き込みも)。
	for _, tc := range []struct{ method, path string }{
		{"GET", evPath(chatTestID, "events")}, {"POST", evPath(chatTestID, "message")}, {"POST", evPath(chatTestID, "permission")}, {"POST", evPath(chatTestID, "stop")},
	} {
		if code, _ := cw.reqWith(anon, tc.method, tc.path, `{}`, nil); code != 401 {
			t.Errorf("無認証 %s %s = %d (401 のはず)", tc.method, tc.path, code)
		}
	}
	// CSRF: Origin・Content-Type・Sec-Fetch-Site。
	msg := evPath(chatTestID, "message")
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"Origin なし", map[string]string{"Origin": ""}, 403},
		{"別の Origin", map[string]string{"Origin": "http://evil.example"}, 403},
		{"Origin が null", map[string]string{"Origin": "null"}, 403},
		{"Origin の後ろにゴミ", map[string]string{"Origin": cw.origin + ".evil.example"}, 403},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"text/plain", map[string]string{"Content-Type": "text/plain"}, 415},
		{"Content-Type なし", map[string]string{"Content-Type": ""}, 415},
		{"form", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 415},
	} {
		if code, b := cw.req("POST", msg, `{"text":"x"}`, tc.headers); code != tc.want {
			t.Errorf("%s: %d %.80s (%d のはず)", tc.name, code, b, tc.want)
		}
	}
	// 壊れたセッション ID (path traversal を含む)。
	for _, id := range []string{"..", "%2e%2e", "x", "20260101-000000-ABCDEF", chatTestID + "/../x", "..%2f..%2fetc", "20260101-000000-abcdef%00"} {
		if code, _ := cw.req("POST", "/s/"+id+"/message", `{"text":"x"}`, nil); code == 200 {
			t.Errorf("壊れた ID %q が通った", id)
		}
		if resp, err := cw.client.Get(cw.web.URL + "/s/" + id + "/events"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Errorf("壊れた ID %q の events が通った", id)
			}
		}
	}
	// 大きすぎる本文は 413 (serve へ流さない)。
	if code, _ := cw.req("POST", msg, `{"text":"`+strings.Repeat("a", chatMaxBody)+`"}`, nil); code != 413 {
		t.Errorf("大きい本文 = %d (413 のはず)", code)
	}
	// method: HEAD /events は serve へ流さない (serve は HEAD でも SSE の枠を使う)。POST /events・GET /message は 405。
	if code, _ := cw.req("HEAD", evPath(chatTestID, "events"), ``, nil); code != 405 {
		t.Errorf("HEAD events = %d (405 のはず)", code)
	}
	if code, _ := cw.req("POST", evPath(chatTestID, "events"), `{}`, nil); code != 405 {
		t.Errorf("POST events = %d", code)
	}
	if code, _ := cw.req("GET", msg, ``, nil); code != 405 {
		t.Errorf("GET message = %d", code)
	}
	if n := l.n.Load(); n != 0 {
		t.Errorf("serve に %d 回、繋いだ (0 のはず)", n)
	}
	// 正しい要求は、通る (関門が、正しい要求を断っていない)。
	if code, b := cw.req("POST", msg, `{"text":"ok"}`, map[string]string{"Content-Type": "Application/JSON; charset=utf-8", "Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Errorf("正しい要求 = %d %s", code, b)
	}
	if l.n.Load() != 1 {
		t.Errorf("serve への接続 = %d (1 のはず)", l.n.Load())
	}
}

// chat.sock が無い (または、前の serve の残りで、繋がらない) とき、起こさずに 404。
func TestWebChatNoSocketIs404AndNeverSpawns(t *testing.T) {
	withTimeout(t, 30*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	if _, code := cw.events(chatTestID); code != 404 {
		t.Errorf("events (chat.sock なし) = %d (404 のはず)", code)
	}
	for _, op := range []string{"message", "permission", "stop"} {
		if code, _ := cw.req("POST", evPath(chatTestID, op), `{}`, nil); code != 404 {
			t.Errorf("%s (chat.sock なし) = %d (404 のはず)", op, code)
		}
	}
	// 前の serve の残り (ソケットのファイルはあるが、待ち受けていない)。
	sock := chatSocketPath(cw.stateDir, defaultGroup, chatTestID2)
	os.MkdirAll(filepath.Dir(sock), 0o700)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, code := cw.events(chatTestID2); code != 404 {
		t.Errorf("events (残りのソケット) = %d (404 のはず)", code)
	}
	// 起こしていない: セッションのディレクトリに、新しいものが作られていない。
	if ents, _ := os.ReadDir(filepath.Join(cw.stateDir, "groups", defaultGroup, "sessions")); len(ents) != 1 {
		t.Errorf("sessions の中身 = %v (残りのソケットの 1 つだけのはず)", ents)
	}
}

// SSE は、http.Server の ReadTimeout・WriteTimeout (どちらも短くする) を超えて生き、心拍が届き、そのあとのイベントも届く。
func TestWebChatSSEOutlivesServerTimeouts(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, fastRelay(), 400*time.Millisecond, 400*time.Millisecond)
	s := newInProcChatSession(t, "hold")
	cw.listen(chatTestID, newChatHandler(s))
	sse := cw.mustEvents(chatTestID)
	sse.next() // hello
	time.Sleep(1500 * time.Millisecond)
	if code, b := cw.req("POST", evPath(chatTestID, "message"), `{"text":"late"}`, nil); code != 200 {
		t.Fatalf("message: %d %s", code, b)
	}
	sse.until(evType("turn.started"))
	if sse.pings < 3 {
		t.Errorf("心拍が %d 個 (3 個以上のはず)", sse.pings)
	}
}

// SSE の同時接続の上限 (セッション単位・全体)。超過は 503 で、ほかの要求 (whoami) を巻き込まない。切ると、枠が戻る。
func TestWebChatSSELimits(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	s := newInProcChatSession(t, "hold")
	h := newChatHandler(s)
	cw.listen(chatTestID, h)
	cw.listen(chatTestID2, h)
	open := func(id string) (*http.Response, context.CancelFunc) {
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, "GET", cw.web.URL+"/s/"+id+"/events", nil)
		resp, err := cw.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, cancel
	}
	r1, c1 := open(chatTestID)
	defer c1()
	_, c2 := open(chatTestID)
	defer c2()
	if r, _ := open(chatTestID); r.StatusCode != 503 { // セッション単位 (2)
		t.Errorf("セッション単位の上限を超えた SSE = %d (503 のはず)", r.StatusCode)
	}
	_, c3 := open(chatTestID2)
	defer c3()
	if r, _ := open(chatTestID2); r.StatusCode != 503 { // 全体 (3)
		t.Errorf("全体の上限を超えた SSE = %d (503 のはず)", r.StatusCode)
	}
	if code, _ := cw.req("GET", "/api/whoami", ``, nil); code != 200 {
		t.Errorf("SSE の枠が埋まった間の whoami = %d", code)
	}
	if code, b := cw.req("POST", evPath(chatTestID, "message"), `{"text":"still"}`, nil); code != 200 {
		t.Errorf("SSE の枠が埋まった間の message = %d %s", code, b)
	}
	c1() // 1 本切ると、枠が戻る
	r1.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, cancel := open(chatTestID)
		ok := r.StatusCode == 200
		defer cancel()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("切った枠が、戻らない")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 偽の上流: h を、id の chat.sock で待ち受ける。
func fakeUpstream(t *testing.T, cw *chatWeb, id string, h http.HandlerFunc) {
	t.Helper()
	cw.listen(id, h)
}

// 読まないクライアント (e) は、書き込みの期限で切られ、枠を握り続けない。上流 (止まらずに書き続ける) の接続も閉じる。
func TestWebChatSSEUnreadingClientIsCut(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	upstreamDone := make(chan struct{})
	var once sync.Once
	fakeUpstream(t, cw, chatTestID, func(w http.ResponseWriter, r *http.Request) {
		defer once.Do(func() { close(upstreamDone) })
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		chunk := "data: " + strings.Repeat("x", 60<<10) + "\n\n"
		for r.Context().Err() == nil {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	// 生の接続で、要求を送って、読まない。
	c, err := net.Dial("tcp", strings.TrimPrefix(cw.web.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var cookie []string
	u, _ := http.NewRequest("GET", cw.web.URL, nil)
	for _, ck := range cw.client.Jar.Cookies(u.URL) {
		cookie = append(cookie, ck.String())
	}
	fmt.Fprintf(c, "GET /s/%s/events HTTP/1.1\r\nHost: x\r\nCookie: %s\r\n\r\n", chatTestID, strings.Join(cookie, "; "))
	select {
	case <-upstreamDone:
	case <-time.After(20 * time.Second):
		t.Fatal("読まないクライアントが、書き込みの期限で切られず、上流の接続が閉じない")
	}
	// 枠が戻っている (セッション単位 2 本まで、また開ける)。
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", cw.web.URL+"/s/"+chatTestID+"/events", nil)
		resp, err := cw.client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("切った後の %d 本目: %v %v", i, resp, err)
		}
	}
}

// 止まった serve (受け付けて、何も返さない) が、web の他の要求・枠を塞がない (g)。
func TestWebChatStalledServeDoesNotBlockWeb(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fakeUpstream(t, cw, chatTestID, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	start := time.Now()
	if code, _ := cw.req("POST", evPath(chatTestID, "message"), `{"text":"x"}`, nil); code != 504 {
		t.Errorf("止まった serve への message = %d (504 のはず)", code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("message が %v かかった", d)
	}
	for i := 0; i < 3; i++ { // 枠 (per-session 2・全体 3) を超える回数、止まった serve への SSE を、続けて開いても、枠が戻る
		if _, code := cw.events(chatTestID); code != 504 {
			t.Errorf("止まった serve への events = %d (504 のはず)", code)
		}
	}
	if code, _ := cw.req("GET", "/api/whoami", ``, nil); code != 200 {
		t.Errorf("whoami = %d", code)
	}
}

// クライアントが切れると、上流 (serve) への接続も閉じる (e)。
func TestWebChatClientDisconnectClosesUpstream(t *testing.T) {
	withTimeout(t, 30*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	up := make(chan struct{})
	fakeUpstream(t, cw, chatTestID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: hello\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(up)
	})
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", cw.web.URL+"/s/"+chatTestID+"/events", nil)
	resp, err := cw.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	select {
	case <-up:
	case <-time.After(10 * time.Second):
		t.Fatal("クライアントが切れても、上流の接続が閉じない")
	}
}

// 上流の SSE のフレーミングが破れたら (知らない行)、そこで止める: クライアントに、別のイベントを偽造させない。
func TestWebChatBrokenUpstreamFramingStops(t *testing.T) {
	withTimeout(t, 30*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	fakeUpstream(t, cw, chatTestID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: hello\ndata: {}\n\nid: 5\nretry: 1\n\ndata: after\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	sse := cw.mustEvents(chatTestID)
	if f, ok := sse.next(); !ok || f.event != "hello" {
		t.Fatalf("hello = %+v %v", f, ok)
	}
	if f, ok := sse.next(); ok {
		t.Errorf("フレーミングが破れた後も、イベントが届いた: %+v", f)
	}
}

// 重複キー・大文字小文字の違うキーの本文が、web 経由と serve 直で、同じ結果になる (L-G。web は本文を解析せず、そのまま渡す)。
func TestWebChatBodyParsingMatchesServe(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, defaultChatRelay, webReadTimeout, webWriteTimeout)
	viaWeb := newInProcChatSession(t, "hold")
	direct := newInProcChatSession(t, "hold")
	cw.listen(chatTestID, newChatHandler(viaWeb))
	dsrv := httptest.NewServer(newChatHandler(direct))
	defer dsrv.Close()

	type tc struct{ name, op, body string }
	gen := func(s *chatSession) string { return s.Chat.Conv.Generation() }
	cases := func(s *chatSession) []tc {
		g := gen(s)
		return []tc{
			{"text 重複 (後が勝つ: 空)", "message", `{"text":"a","text":""}`},
			{"text 大文字小文字 (空)", "message", `{"TEXT":""}`},
			{"text 混在", "message", `{"Text":"x","text":""}`},
			{"未知のフィールド", "message", `{"text":"a","extra":1}`},
			{"後ろのゴミ", "message", `{"text":"a"} x`},
			{"世代の重複 (後が勝つ: 別)", "permission", `{"generation":"` + g + `","generation":"bad","request_id":"r","outcome":"allow_once"}`},
			{"世代の重複 (後が勝つ: 本物・未知の要求)", "permission", `{"generation":"bad","generation":"` + g + `","request_id":"r","outcome":"allow_once"}`},
			{"キーの大文字小文字", "permission", `{"GENERATION":"` + g + `","Request_ID":"r","OUTCOME":"allow_once"}`},
			{"キーの大文字小文字 (世代が別)", "permission", `{"GENERATION":"bad","Request_ID":"r","OUTCOME":"allow_once"}`},
			{"outcome の重複", "permission", `{"generation":"` + g + `","request_id":"r","outcome":"allow_always","outcome":"allow_once"}`},
			{"stop の未知のフィールド", "stop", `{"x":1}`},
		}
	}
	web, dir := cases(viaWeb), cases(direct)
	for i := range web {
		wc, wb := cw.req("POST", evPath(chatTestID, web[i].op), web[i].body, nil)
		req, _ := http.NewRequest("POST", dsrv.URL+"/"+dir[i].op, strings.NewReader(dir[i].body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		db, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if wc != resp.StatusCode || wb != string(db) {
			t.Errorf("%s: web 経由 %d %s / serve 直 %d %s", web[i].name, wc, wb, resp.StatusCode, db)
		}
	}
}

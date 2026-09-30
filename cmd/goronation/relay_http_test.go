package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRelayHost = "127.0.0.1:4096"

func parse(t *testing.T, raw string) (*relayRequest, error) {
	t.Helper()
	return parseRequest(bufio.NewReaderSize(strings.NewReader(raw), relayMaxLine), testRelayHost, basicAuth("TOKEN"))
}

// headLines は、上流に書く先頭を、ヘッダ名 (小文字) → 値の一覧にする。
func headLines(t *testing.T, head []byte) (first string, h map[string][]string) {
	t.Helper()
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(head)))
	first, err := r.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	mh, err := r.ReadMIMEHeader()
	if err != nil {
		t.Fatal(err)
	}
	h = map[string][]string{}
	for k, v := range mh {
		h[strings.ToLower(k)] = v
	}
	return first, h
}

// ヘッダの付け直し: クライアントが付けた認証・経路のヘッダは、どんな書き方でも、上流の先頭に 1 つも残らず、init の Authorization・Host が 1 つずつだけ付く。
func TestParseRequestRewritesAuthorization(t *testing.T) {
	auth := basicAuth("TOKEN")
	for _, tc := range []struct{ name, extra string }{
		{"なし", ""},
		{"1 つ", "Authorization: Bearer forged\r\n"},
		{"小文字", "authorization: Bearer forged\r\n"},
		{"大文字", "AUTHORIZATION: Bearer forged\r\n"},
		{"重複 (先頭に足す)", "Authorization: Basic eDp5\r\nAuthorization: Bearer forged\r\n"},
		{"空の値", "Authorization:\r\n"},
		{"値の前後の空白", "Authorization:   \t Bearer forged \t \r\n"},
		{"Proxy-Authorization", "Proxy-Authorization: Basic eDp5\r\n"},
		{"Host の偽装", "Host: evil.example\r\nHost: 127.0.0.1:1\r\n"},
		{"Connection: keep-alive", "Connection: keep-alive\r\nKeep-Alive: timeout=99\r\nProxy-Connection: keep-alive\r\n"},
		{"X-Forwarded-*", "X-Forwarded-For: 1.2.3.4\r\nX-Forwarded-Host: evil\r\nForwarded: for=1.2.3.4\r\nX-Real-IP: 1.2.3.4\r\nVia: 1.1 evil\r\n"},
		{"TE・Trailer", "TE: trailers\r\nTrailer: X\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parse(t, "GET /api/info HTTP/1.1\r\nAccept: */*\r\n"+tc.extra+"\r\n")
			if err != nil {
				t.Fatalf("拒否された: %v", err)
			}
			first, h := headLines(t, req.head)
			if first != "GET /api/info HTTP/1.1" {
				t.Errorf("要求行 = %q", first)
			}
			if !equal(h["authorization"], auth) || !equal(h["host"], testRelayHost) || !equal(h["connection"], "close") {
				t.Errorf("authorization=%q host=%q connection=%q", h["authorization"], h["host"], h["connection"])
			}
			for _, k := range []string{"proxy-authorization", "x-forwarded-for", "x-forwarded-host", "forwarded", "x-real-ip", "via", "te", "trailer", "keep-alive", "proxy-connection"} {
				if len(h[k]) != 0 {
					t.Errorf("%s が残った: %q", k, h[k])
				}
			}
			if strings.Contains(string(req.head), "forged") || strings.Contains(string(req.head), "evil") {
				t.Errorf("クライアントの値が残った:\n%s", req.head)
			}
			if !equal(h["accept"], "*/*") {
				t.Errorf("ほかのヘッダは通る: accept=%q", h["accept"])
			}
		})
	}
}

func equal(got []string, want string) bool { return len(got) == 1 && got[0] == want }

// 拒否: 形が不正・通さない要求は、上流に何も書かずに、理由の status で拒否する。
func TestParseRequestRejects(t *testing.T) {
	longLine := "X: " + strings.Repeat("a", relayMaxLine) + "\r\n"
	manyHeaders := strings.Repeat("X-A: 1\r\n", relayMaxHeaders+1)
	bigHead := strings.Repeat("X-A: "+strings.Repeat("b", 4000)+"\r\n", 10)
	for _, tc := range []struct {
		name, raw string
		status    int
	}{
		{"absolute-form", "GET http://evil.example/api/info HTTP/1.1\r\n\r\n", 400},
		{"CONNECT", "CONNECT 127.0.0.1:22 HTTP/1.1\r\n\r\n", 405},
		{"OPTIONS", "OPTIONS /api/info HTTP/1.1\r\n\r\n", 405},
		{"HEAD", "HEAD /api/info HTTP/1.1\r\n\r\n", 405},
		{"HTTP/1.0", "GET /api/info HTTP/1.0\r\n\r\n", 505},
		{"HTTP/2", "GET /api/info HTTP/2\r\n\r\n", 505},
		{"/api/ の外", "GET /doc HTTP/1.1\r\n\r\n", 400},
		{"/api の前置", "GET /apix/info HTTP/1.1\r\n\r\n", 400},
		{"// の始まり", "GET //api/info HTTP/1.1\r\n\r\n", 400},
		{"target に空白", "GET /api/a b HTTP/1.1\r\n\r\n", 400},
		{"target に #", "GET /api/a#b HTTP/1.1\r\n\r\n", 400},
		{"target に \\", "GET /api/a\\b HTTP/1.1\r\n\r\n", 400},
		{"target に NUL", "GET /api/a\x00b HTTP/1.1\r\n\r\n", 400},
		{"要求行が 2 つ組", "GET /api/info\r\n\r\n", 400},
		{"要求行が 4 つ組", "GET /api/info HTTP/1.1 x\r\n\r\n", 400},
		{"要求行に二重の空白", "GET  /api/info HTTP/1.1\r\n\r\n", 400},
		{"LF だけ", "GET /api/info HTTP/1.1\nHost: x\n\n", 400},
		{"ヘッダの LF だけ", "GET /api/info HTTP/1.1\r\nX: 1\nY: 2\r\n\r\n", 400},
		{"obs-fold", "GET /api/info HTTP/1.1\r\nX: 1\r\n Authorization: y\r\n\r\n", 400},
		{"obs-fold (HT)", "GET /api/info HTTP/1.1\r\nX: 1\r\n\tfoo\r\n\r\n", 400},
		{"コロンが無い", "GET /api/info HTTP/1.1\r\nFoo\r\n\r\n", 400},
		{"ヘッダ名が空", "GET /api/info HTTP/1.1\r\n: x\r\n\r\n", 400},
		{"ヘッダ名に空白", "GET /api/info HTTP/1.1\r\nFoo : x\r\n\r\n", 400},
		{"ヘッダ名に NUL", "GET /api/info HTTP/1.1\r\nFo\x00o: x\r\n\r\n", 400},
		{"ヘッダの値に NUL", "GET /api/info HTTP/1.1\r\nFoo: a\x00b\r\n\r\n", 400},
		{"ヘッダの値に CR", "GET /api/info HTTP/1.1\r\nFoo: a\rb\r\n\r\n", 400},
		{"ヘッダの値が非 ASCII", "GET /api/info HTTP/1.1\r\nFoo: é\r\n\r\n", 400},
		{"Transfer-Encoding", "POST /api/x HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", 501},
		{"Transfer-Encoding と Content-Length", "POST /api/x HTTP/1.1\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n", 501},
		{"TE の大文字小文字", "POST /api/x HTTP/1.1\r\ntransfer-ENCODING : chunked\r\n\r\n", 400},
		{"Upgrade", "GET /api/event HTTP/1.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", 400},
		{"Expect", "POST /api/x HTTP/1.1\r\nExpect: 100-continue\r\nContent-Length: 1\r\n\r\n", 417},
		{"Content-Length の重複 (同じ値)", "POST /api/x HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\n", 400},
		{"Content-Length の重複 (違う値)", "POST /api/x HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n", 400},
		{"Content-Length のコンマ", "POST /api/x HTTP/1.1\r\nContent-Length: 1, 1\r\n\r\n", 400},
		{"Content-Length が負", "POST /api/x HTTP/1.1\r\nContent-Length: -1\r\n\r\n", 400},
		{"Content-Length に +", "POST /api/x HTTP/1.1\r\nContent-Length: +1\r\n\r\n", 400},
		{"Content-Length が空", "POST /api/x HTTP/1.1\r\nContent-Length:\r\n\r\n", 400},
		{"Content-Length が 16 進", "POST /api/x HTTP/1.1\r\nContent-Length: 0x10\r\n\r\n", 400},
		{"Content-Length が桁あふれ", "POST /api/x HTTP/1.1\r\nContent-Length: 99999999999999999999\r\n\r\n", 400},
		{"Content-Length が上限超え", "POST /api/x HTTP/1.1\r\nContent-Length: " + strconv.Itoa(relayMaxBody+1) + "\r\n\r\n", 413},
		{"長い 1 行", "GET /api/info HTTP/1.1\r\n" + longLine + "\r\n", 431},
		{"ヘッダが多すぎる", "GET /api/info HTTP/1.1\r\n" + manyHeaders + "\r\n", 431},
		{"先頭が大きすぎる", "GET /api/info HTTP/1.1\r\n" + bigHead + "\r\n", 431},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parse(t, tc.raw)
			var re *relayError
			if !errors.As(err, &re) || re.status != tc.status {
				t.Fatalf("got req=%v err=%v, want status %d", req != nil, err, tc.status)
			}
		})
	}
}

// Content-Length は、1 つだけ・数字だけ・上限以下。それ以外の本文の長さの示し方は、通らない。本文の長さを示さない POST は 0 にそろえる。
func TestParseRequestContentLength(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int64
	}{
		{"なし", "POST /api/x HTTP/1.1\r\n\r\n", 0},
		{"0", "POST /api/x HTTP/1.1\r\nContent-Length: 0\r\n\r\n", 0},
		{"12", "POST /api/x HTTP/1.1\r\nCONTENT-LENGTH:  12 \r\n\r\n", 12},
		{"上限", "POST /api/x HTTP/1.1\r\nContent-Length: " + strconv.Itoa(relayMaxBody) + "\r\n\r\n", relayMaxBody},
		{"GET に本文", "GET /api/x HTTP/1.1\r\nContent-Length: 3\r\n\r\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parse(t, tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			_, h := headLines(t, req.head)
			if req.bodyLen != tc.want || !equal(h["content-length"], strconv.FormatInt(tc.want, 10)) {
				t.Errorf("bodyLen=%d content-length=%q, want %d", req.bodyLen, h["content-length"], tc.want)
			}
		})
	}
}

// 分類: GET /api/event (クエリは除く) だけが、SSE の枠に入る。
func TestRequestIsStream(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"GET /api/event HTTP/1.1\r\n\r\n", true},
		{"GET /api/event?x=1 HTTP/1.1\r\n\r\n", true},
		{"POST /api/event HTTP/1.1\r\n\r\n", false},
		{"GET /api/events HTTP/1.1\r\n\r\n", false},
		{"GET /api/event/ HTTP/1.1\r\n\r\n", false},
		{"GET /api/info HTTP/1.1\r\n\r\n", false},
	} {
		req, err := parse(t, tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if req.isStream() != tc.want {
			t.Errorf("%q: isStream=%v, want %v", strings.SplitN(tc.raw, "\r\n", 2)[0], req.isStream(), tc.want)
		}
	}
}

// FuzzParseRequest: どんな入力でも、パニックせず、通った要求の先頭は、認証・経路のヘッダの偽装を含まず、Authorization・Host・Connection・Content-Length が
// ちょうど 1 つずつで、CR・LF・NUL が、行の区切り以外に入らない (ヘッダの注入)。
func FuzzParseRequest(f *testing.F) {
	for _, s := range []string{
		"GET /api/info HTTP/1.1\r\n\r\n",
		"POST /api/session HTTP/1.1\r\nAuthorization: Bearer x\r\nContent-Length: 2\r\n\r\n{}",
		"GET /api/event HTTP/1.1\r\nauthorization : x\r\n\r\n",
		"GET /api/x HTTP/1.1\r\nX: 1\r\n Authorization: y\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n",
		"GET /api/x HTTP/1.1\nAuthorization: y\n\n",
	} {
		f.Add(s)
	}
	auth := basicAuth("TOKEN")
	f.Fuzz(func(t *testing.T, raw string) {
		req, err := parseRequest(bufio.NewReaderSize(strings.NewReader(raw), relayMaxLine), testRelayHost, auth)
		if err != nil {
			return
		}
		head := string(req.head)
		if !strings.HasSuffix(head, "\r\n\r\n") {
			t.Fatalf("先頭が閉じていない: %q", head)
		}
		lines := strings.Split(strings.TrimSuffix(head, "\r\n\r\n"), "\r\n")
		counts := map[string]int{}
		for i, l := range lines {
			if strings.ContainsAny(l, "\r\n\x00") {
				t.Fatalf("行に CR・LF・NUL: %q", l)
			}
			if i == 0 {
				continue
			}
			name, _, ok := strings.Cut(l, ":")
			if !ok {
				t.Fatalf("ヘッダの形が不正: %q", l)
			}
			counts[strings.ToLower(name)]++
		}
		for _, k := range []string{"authorization", "host", "connection", "content-length"} {
			if counts[k] != 1 {
				t.Fatalf("%s が %d 個: %q (入力 %q)", k, counts[k], head, raw)
			}
		}
		for _, k := range []string{"proxy-authorization", "transfer-encoding", "upgrade", "expect", "te", "x-forwarded-for", "forwarded"} {
			if counts[k] != 0 {
				t.Fatalf("%s が残った: %q (入力 %q)", k, head, raw)
			}
		}
		if !strings.Contains(head, "\r\nAuthorization: "+auth+"\r\n") || !strings.Contains(head, "\r\nHost: "+testRelayHost+"\r\n") {
			t.Fatalf("init の値が付いていない: %q", head)
		}
	})
}

// ---- 上流までの通し (偽の上流の TCP サーバー。受けた要求の全体を記録する) ----

// relayUpstream は、127.0.0.1 で待ち受け、接続ごとに、要求の先頭と本文 (Content-Length 分) を読んで記録し、respond の応答を返す。
type relayUpstream struct {
	t       *testing.T
	l       net.Listener
	mu      sync.Mutex
	reqs    []string // 記録した要求 (先頭 + 本文)
	extra   []string // 本文の後に届いた余計なバイト
	closed  chan struct{}
	respond func(c net.Conn, req string)
}

func newRelayUpstream(t *testing.T, respond func(c net.Conn, req string)) *relayUpstream {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &relayUpstream{t: t, l: l, respond: respond, closed: make(chan struct{}, 64)}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go u.serve(c)
		}
	}()
	return u
}

func (u *relayUpstream) port() int { return u.l.Addr().(*net.TCPAddr).Port }

func (u *relayUpstream) serve(c net.Conn) {
	defer func() { c.Close(); u.closed <- struct{}{} }()
	br := bufio.NewReader(c)
	var head strings.Builder
	cl := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		head.WriteString(line)
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":"); ok && strings.EqualFold(k, "content-length") {
			cl, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if line == "\r\n" {
			break
		}
	}
	body := make([]byte, cl)
	if _, err := io.ReadFull(br, body); err != nil {
		return
	}
	req := head.String() + string(body)
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	rest, _ := io.ReadAll(br) // 本文の後に、何か届くか (届いてはいけない)
	u.mu.Lock()
	u.reqs = append(u.reqs, req)
	if len(rest) > 0 {
		u.extra = append(u.extra, string(rest))
	}
	u.mu.Unlock()
	c.SetReadDeadline(time.Time{})
	u.respond(c, req)
}

func (u *relayUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.reqs...)
}

func okResponse(body string) func(net.Conn, string) {
	return func(c net.Conn, _ string) {
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	}
}

// relayPair は、net.Pipe でない、実 fd の組 (要求を書く側と、relay が読む側) を作る。
func relayPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); ch <- c }()
	client, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-ch
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

func readAll(t *testing.T, c net.Conn) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(testTimeout))
	b, _ := io.ReadAll(c)
	return string(b)
}

// 通し: 偽造した Authorization・Host は上流に届かず、init のトークンの認証だけが付き、本文はちょうど Content-Length 分が届く。応答は解釈せず、そのまま返る。
func TestRequestRelayEndToEnd(t *testing.T) {
	up := newRelayUpstream(t, okResponse("pong"))
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	cl, sv := relayPair(t)
	r.accept(sv)
	fmt.Fprintf(cl, "POST /api/session/ses_1/prompt HTTP/1.1\r\nHost: evil\r\nauthorization: Bearer forged\r\nContent-Type: application/json\r\nContent-Length: 13\r\n\r\n{\"text\":\"go\"}\r\n")
	got := readAll(t, cl)
	if got != "HTTP/1.1 200 OK\r\nContent-Length: 4\r\nConnection: close\r\n\r\npong" {
		t.Errorf("応答 = %q", got)
	}
	reqs := up.requests()
	if len(reqs) != 1 {
		t.Fatalf("上流が受けた要求 = %d 個", len(reqs))
	}
	first, h := headLines(t, []byte(strings.SplitN(reqs[0], "\r\n\r\n", 2)[0]+"\r\n\r\n"))
	body := strings.SplitN(reqs[0], "\r\n\r\n", 2)[1]
	if first != "POST /api/session/ses_1/prompt HTTP/1.1" || body != `{"text":"go"}` {
		t.Errorf("要求行 = %q, 本文 = %q", first, body)
	}
	if !equal(h["authorization"], basicAuth("TOKEN")) || !equal(h["host"], "127.0.0.1:"+strconv.Itoa(up.port())) {
		t.Errorf("authorization=%q host=%q", h["authorization"], h["host"])
	}
	if !equal(h["content-type"], "application/json") {
		t.Errorf("content-type=%q", h["content-type"])
	}
}

// 1 fd = 1 要求: 本文の後に、2 つ目の要求 (pipelining) を書き足しても、上流に届かない。
func TestRequestRelayNoPipelining(t *testing.T) {
	up := newRelayUpstream(t, okResponse("a"))
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	cl, sv := relayPair(t)
	r.accept(sv)
	fmt.Fprintf(cl, "GET /api/info HTTP/1.1\r\n\r\nGET /api/secret HTTP/1.1\r\nAuthorization: Bearer forged\r\n\r\n")
	readAll(t, cl)
	reqs := up.requests()
	if len(reqs) != 1 || strings.Contains(reqs[0], "secret") {
		t.Fatalf("上流が受けた要求 = %q", reqs)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.extra) != 0 {
		t.Errorf("本文の後の余計なバイトが上流に届いた: %q", up.extra)
	}
}

// 本文が、Content-Length より長く書かれても、余りは届かない。短ければ (途中で止まれば) 上流に中途半端な要求を流さず、拒否する。
func TestRequestRelayBodyLength(t *testing.T) {
	up := newRelayUpstream(t, okResponse("a"))
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	cl, sv := relayPair(t)
	r.accept(sv)
	fmt.Fprintf(cl, "POST /api/x HTTP/1.1\r\nContent-Length: 3\r\n\r\nabcdefGET /api/y HTTP/1.1\r\n\r\n")
	readAll(t, cl)
	reqs := up.requests()
	if len(reqs) != 1 || !strings.HasSuffix(reqs[0], "\r\n\r\nabc") {
		t.Fatalf("上流が受けた要求 = %q", reqs)
	}
}

// 拒否された要求は、上流に何も届かない (接続もされない)。ホストには、理由の status が返る。
func TestRequestRelayRejectNeverReachesUpstream(t *testing.T) {
	up := newRelayUpstream(t, okResponse("a"))
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	for _, raw := range []string{
		"GET http://x/api/info HTTP/1.1\r\n\r\n",
		"POST /api/x HTTP/1.1\r\nTransfer-Encoding: chunked\r\nContent-Length: 4\r\n\r\n0\r\n\r\n",
		"CONNECT 127.0.0.1:22 HTTP/1.1\r\n\r\n",
	} {
		cl, sv := relayPair(t)
		r.accept(sv)
		io.WriteString(cl, raw)
		got := readAll(t, cl)
		if !strings.HasPrefix(got, "HTTP/1.1 4") && !strings.HasPrefix(got, "HTTP/1.1 501") {
			t.Errorf("%q への応答 = %q", raw, got)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("拒否した要求が上流に届いた: %d 個", n)
	}
}

// SSE: 上流の応答は、バッファせずに、すぐ返る。ホストが閉じると、上流の接続も閉じる。
func TestRequestRelayStreamsAndCancels(t *testing.T) {
	up := newRelayUpstream(t, func(c net.Conn, _ string) {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\ndata: one\n\n")
		io.Copy(io.Discard, c) // 閉じられるまで待つ
	})
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	cl, sv := relayPair(t)
	r.accept(sv)
	io.WriteString(cl, "GET /api/event HTTP/1.1\r\nAccept: text/event-stream\r\n\r\n")
	cl.SetReadDeadline(time.Now().Add(testTimeout))
	br := bufio.NewReader(cl)
	var seen strings.Builder
	for !strings.Contains(seen.String(), "data: one") {
		line, err := br.ReadString('\n') // 上流がまだ応答を閉じていないのに、読める
		if err != nil {
			t.Fatalf("SSE の最初のイベントが届かない: %v (%q)", err, seen.String())
		}
		seen.WriteString(line)
	}
	cl.Close() // ホストが取り消す
	select {
	case <-up.closed:
	case <-time.After(testTimeout):
		t.Fatal("ホストが閉じても、上流の接続が閉じない")
	}
}

// 同時数: SSE の枠が埋まっても、短い要求は通る。SSE の枠を超える要求は 503。全体の枠を超えた fd は、何も読まずに閉じる。
func TestRequestRelayLimits(t *testing.T) {
	up := newRelayUpstream(t, func(c net.Conn, req string) {
		if strings.HasPrefix(req, "GET /api/event") {
			io.WriteString(c, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
			io.Copy(io.Discard, c)
			return
		}
		okResponse("short")(c, req)
	})
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	var streams []net.Conn
	for i := 0; i < relayMaxStreams; i++ {
		cl, sv := relayPair(t)
		r.accept(sv)
		io.WriteString(cl, "GET /api/event HTTP/1.1\r\n\r\n")
		br := bufio.NewReader(cl)
		cl.SetReadDeadline(time.Now().Add(testTimeout))
		if l, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(l, "HTTP/1.1 200") {
			t.Fatalf("SSE %d: %q %v", i, l, err)
		}
		streams = append(streams, cl)
	}
	over, sv := relayPair(t)
	r.accept(sv)
	io.WriteString(over, "GET /api/event HTTP/1.1\r\n\r\n")
	if got := readAll(t, over); !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Errorf("SSE の枠の超過 = %q, want 503", got)
	}
	short, sv2 := relayPair(t)
	r.accept(sv2)
	io.WriteString(short, "GET /api/info HTTP/1.1\r\n\r\n")
	if got := readAll(t, short); !strings.HasSuffix(got, "short") {
		t.Errorf("SSE の枠が埋まっている間の短い要求 = %q", got)
	}
	for _, c := range streams {
		c.Close()
	}
}

func TestRequestRelayInflightLimit(t *testing.T) {
	block := make(chan struct{})
	up := newRelayUpstream(t, okResponse("x"))
	_ = block
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	// 先頭を送らない (検査中のまま居座る) fd で、全体の枠を埋める。
	var holders []net.Conn
	for i := 0; i < relayMaxInflight; i++ {
		cl, sv := relayPair(t)
		r.accept(sv)
		holders = append(holders, cl)
	}
	cl, sv := relayPair(t)
	r.accept(sv) // 枠が無い: 何も読まずに閉じる
	cl.SetReadDeadline(time.Now().Add(testTimeout))
	if n, err := cl.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("枠の超過の fd が、閉じられない: n=%d err=%v", n, err)
	}
	for _, c := range holders {
		c.Close()
	}
}

// 上流に繋がらないと、ホストに 502。init (PID 1) は、要求の処理のパニックで落ちない。
func TestRequestRelayBadGatewayAndPanic(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // 何も待ち受けていないポート
	r := newRequestRelay(port, func() string { return "TOKEN" })
	cl, sv := relayPair(t)
	r.accept(sv)
	io.WriteString(cl, "GET /api/info HTTP/1.1\r\n\r\n")
	if got := readAll(t, cl); !strings.HasPrefix(got, "HTTP/1.1 502") {
		t.Errorf("上流が無いときの応答 = %q", got)
	}

	panicky := newRequestRelay(port, func() string { panic("token 供給元の失敗") })
	cl, sv = relayPair(t)
	panicky.accept(sv) // recover されて、fd だけが閉じる
	cl.SetReadDeadline(time.Now().Add(testTimeout))
	if _, err := cl.Read(make([]byte, 1)); err == nil {
		t.Error("パニックした要求の fd が閉じられない")
	}
	panicky.wg.Wait()
}

func newRequest(method, url, body string) (*http.Request, error) {
	return http.NewRequest(method, url, strings.NewReader(body))
}

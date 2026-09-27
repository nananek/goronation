package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// dialLine は、l に接続し、line (末尾に "\r\n" を付けて) を送る。応答の最初の 1 行 (状態行) を返す。
func dialLine(t testing.TB, addr, line string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(line + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func newTestListener(t testing.TB) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// TestServeBothDispatchesConnect は、CONNECT が、既存の Server (connect) の処理 (許可リスト・監査) に、そのまま届くことを確かめる。
func TestServeBothDispatchesConnect(t *testing.T) {
	var auditBuf strings.Builder
	var mu sync.Mutex
	srv := New(Config{Allow: []string{"example.invalid:443"}, Audit: writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		auditBuf.Write(p)
		return len(p), nil
	})})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("git ハンドラが呼ばれた: %s %s", r.Method, r.RequestURI)
	})
	go ServeBoth(l, srv, other, "/git/", "/github-api/")

	// 許可していない宛先: dial-check より前の allowlist で 403 になる (実際に繋ぎに行かない)。
	status := dialLine(t, l.Addr().String(), "CONNECT not-allowed.invalid:443 HTTP/1.1")
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("status = %q", status)
	}
	mu.Lock()
	got := auditBuf.String()
	mu.Unlock()
	if !strings.Contains(got, `"event":"deny"`) || !strings.Contains(got, "not-allowed.invalid:443") {
		t.Fatalf("監査に、CONNECT の拒否が出ていない: %q", got)
	}
}

// TestServeBothDispatchesHTTP は、git・github-api の接頭辞に一致する GET・POST が、other (http.Handler) に届くことを確かめる。
func TestServeBothDispatchesHTTP(t *testing.T) {
	var got []string
	var mu sync.Mutex
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.RequestURI)
		mu.Unlock()
		w.WriteHeader(299)
	})
	connect := New(Config{Allow: nil, Audit: io.Discard})
	l := newTestListener(t)
	go ServeBoth(l, connect, other, "/git/", "/github-api/")

	cases := []string{
		"GET /git/github.com/o/r.git/info/refs?service=git-upload-pack HTTP/1.1",
		"POST /git/github.com/o/r.git/git-receive-pack HTTP/1.1",
		"POST /github-api/repos/o/r/pulls HTTP/1.1",
	}
	for _, line := range cases {
		status := dialLine(t, l.Addr().String(), line+"\r\nHost: x\r\nContent-Length: 0")
		if !strings.HasPrefix(status, "HTTP/1.1 299") {
			t.Fatalf("%s: status = %q", line, status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(cases) {
		t.Fatalf("届いた要求 = %v", got)
	}
	for i, line := range cases {
		want := strings.TrimSuffix(strings.Split(line, " HTTP/")[0], "")
		if got[i] != want {
			t.Errorf("[%d] got %q, want %q", i, got[i], want)
		}
	}
}

// TestServeBothRejectsOther は、接頭辞に一致しない method・path や、CONNECT でも GET/POST でもない method を、
// どちらのハンドラにも渡さず、400 で閉じることを確かめる。
func TestServeBothRejectsOther(t *testing.T) {
	connect := New(Config{Audit: io.Discard})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("呼ばれるはずがない: %s %s", r.Method, r.RequestURI)
	})
	go ServeBoth(l, connect, other, "/git/", "/github-api/")

	// "/git/../etc" のような、接頭辞の後の中身までは、ここでは見ない (文字列の前方一致だけ。".." などの意味は、
	// 渡した先の git.ParseRoute の仕事。PR ① の route_test.go で確かめている)。
	for _, line := range []string{
		"GET /other/path HTTP/1.1\r\nHost: x",
		"GET /gitx HTTP/1.1\r\nHost: x", // 接頭辞 "/git/" に前方一致しない (紛らわしい名前)
		"PUT /git/github.com/o/r.git/git-receive-pack HTTP/1.1\r\nHost: x\r\nContent-Length: 0",
		"DELETE /github-api/repos/o/r/pulls HTTP/1.1\r\nHost: x",
		"GARBAGE",
		"",
	} {
		status := dialLine(t, l.Addr().String(), line)
		if !strings.HasPrefix(status, "HTTP/1.1 400") {
			t.Errorf("%q: status = %q", line, status)
		}
	}
}

func TestHasAnyPrefix(t *testing.T) {
	if hasAnyPrefix("/git/x", nil) {
		t.Error("prefixes が空なら、何にも一致しないはず")
	}
	if hasAnyPrefix("/git/x", []string{}) {
		t.Error("prefixes が空 (nil でない) でも、何にも一致しないはず")
	}
	if !hasAnyPrefix("/git/x", []string{"/other/", "/git/"}) {
		t.Error("一致するはず")
	}
	if hasAnyPrefix("/gitx", []string{"/git/"}) {
		t.Error("接頭辞でない一致 (/gitx が /git/ を含む) を、一致とした")
	}
}

// TestServeBothLongLine は、最初の行が上限を超えるとき、パニックせず 400 で閉じることを確かめる。
func TestServeBothLongLine(t *testing.T) {
	connect := New(Config{Audit: io.Discard})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("呼ばれるはずがない") })
	go ServeBoth(l, connect, other, "/git/")

	status := dialLine(t, l.Addr().String(), "GET /git/"+strings.Repeat("a", maxSniffLine*2)+" HTTP/1.1")
	if !strings.HasPrefix(status, "HTTP/1.1 400") {
		t.Fatalf("status = %q", status)
	}
}

// TestServeBothHTTPHeaderTimeoutEnforced は、攻撃者視点レビュー (attack-review-bc6d03a) の再現テスト。
//
// ServeBoth は、CONNECT 用に Config.HeaderTimeout (ヘッダを読み終えるまでの期限) を持つが、この期限は
// dispatch の「最初の行」だけに使われ、その後 c.SetReadDeadline(time.Time{}) で消える。git・PR (other
// http.Handler) へ振り分けられた接続を Serve する httpSrv に ReadHeaderTimeout 等が架かっていなければ、
// 最初の行 (接頭辞に一致する GET/POST) だけ送って、その後ヘッダの終端 (空行) を決して送らないクライアント
// を、HeaderTimeout を過ぎても切れない (Slowloris)。ServeBoth が httpSrv にも connect.cfg.HeaderTimeout
// を適用していれば、このテストは通る。
func TestServeBothHTTPHeaderTimeoutEnforced(t *testing.T) {
	const headerTimeout = 100 * time.Millisecond
	connect := New(Config{Audit: io.Discard, HeaderTimeout: headerTimeout})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("ハンドラが呼ばれるはずがない (ヘッダの終わりを送っていない): %s %s", r.Method, r.RequestURI)
	})
	go ServeBoth(l, connect, other, "/git/")

	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 接頭辞に一致する最初の行だけ送り、ヘッダの終わり (空行) は決して送らない。
	if _, err := c.Write([]byte("GET /git/x HTTP/1.1\r\n")); err != nil {
		t.Fatal(err)
	}
	// HeaderTimeout の 20 倍待つ。CONNECT 経路と同じ保護が効いていれば、この間に閉じられる (EOF か、
	// 408 などの応答) はず。
	c.SetReadDeadline(time.Now().Add(20 * headerTimeout))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	switch {
	case err == nil:
		if n == 0 {
			t.Fatal("0 バイトの応答")
		}
		// 期限切れ・エラーの応答なら OK (400/408 など)。200 系なら別問題なので、ここでは失敗にしない。
		return
	case err == io.EOF:
		return // サーバがヘッダの締め切りで接続を閉じた: 期待どおり
	case os.IsTimeout(err):
		t.Fatalf("HeaderTimeout (%s) の 20 倍待っても、git/PR 経路の接続が切られなかった (Slowloris 耐性が無い)", headerTimeout)
	default:
		t.Fatalf("予期しない error: %v", err)
	}
}

// TestServeBothIdleTimeoutEnforced は、git/PR 経路の接続 (keep-alive) が、1 回のやり取りの後、
// connect.cfg.IdleTimeout を過ぎても次の要求を送らないまま、接続が保持され続けないことを確かめる
// (attack-review-bc6d03a の申し送りにある IdleTimeout の検討分)。
func TestServeBothIdleTimeoutEnforced(t *testing.T) {
	const idleTimeout = 100 * time.Millisecond
	connect := New(Config{Audit: io.Discard, IdleTimeout: idleTimeout})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	go ServeBoth(l, connect, other, "/git/")

	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /git/x HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("status = %q", status)
	}
	// 1 回目の要求・応答の後、2 回目を送らずに放置する。IdleTimeout の 20 倍待っても接続が生きたままなら失敗。
	c.SetReadDeadline(time.Now().Add(20 * idleTimeout))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	switch {
	case err == io.EOF:
		return // IdleTimeout でサーバが閉じた: 期待どおり
	case os.IsTimeout(err):
		t.Fatalf("IdleTimeout (%s) の 20 倍待っても、keep-alive の接続が保持され続けた", idleTimeout)
	default:
		t.Fatalf("予期しない結果: n=%d err=%v", n, err)
	}
}

// TestServeBothHTTPBodyStallEnforced は、攻撃者視点レビュー (attack-review-d9fe8c5) の再現テスト (B2)。
//
// ヘッダは完全かつ即座に送るが、宣言した Content-Length の本文を 1 バイトも送らないクライアントは、
// ReadHeaderTimeout (ヘッダはもう読み終えている)・IdleTimeout (ハンドラが実行中で、次の要求を待つ区間に
// 入らない) のどちらの対象にもならない。stallGuardBody が無ければ、r.Body.Read はブロックしたまま戻らず、
// ハンドラの goroutine が無期限に残る (本文フェーズの Slowloris)。
//
// ハンドラは、実物の gateway.serveReceivePack などと同じく、本文の読み取りエラーをそのまま返す (無視して
// 200 を書いたりしない)。このテストが確かめたいのは「goroutine が timeout 程度で解放されるか」であって、
// 接続がどう終わるか (EOF か、エラー応答の後の close か) までは問わない (TestServeBothHTTPHeaderTimeoutEnforced
// と同じ判定の形)。
func TestServeBothHTTPBodyStallEnforced(t *testing.T) {
	const timeout = 100 * time.Millisecond
	connect := New(Config{Audit: io.Discard, HeaderTimeout: timeout, IdleTimeout: timeout})
	l := newTestListener(t)
	bodyReadStarted := make(chan struct{}, 1)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyReadStarted <- struct{}{}
		buf := make([]byte, 100000)
		if _, err := r.Body.Read(buf); err != nil {
			http.Error(w, "body stalled", http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(200)
	})
	go ServeBoth(l, connect, other, "/git/")

	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// ヘッダは完全に・即座に送る (Content-Length: 100000 を宣言するが、本文は 1 バイトも送らない)。
	req := "POST /git/x HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bodyReadStarted:
	case <-time.After(time.Second):
		t.Fatal("ハンドラが呼ばれなかった (想定外)")
	}
	// 締め切りの 20 倍待つ。stallGuardBody が効いていれば、この間に (EOF か、エラー応答か) 決着が付くはず。
	c.SetReadDeadline(time.Now().Add(20 * timeout))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	switch {
	case err == nil:
		if n == 0 {
			t.Fatal("0 バイトの応答")
		}
		// エラー応答 (408 など) が返ってくれば OK: ハンドラの goroutine は、本文を待ち続けていない。
		return
	case err == io.EOF:
		return // サーバが接続を閉じた: 期待どおり
	case os.IsTimeout(err):
		t.Fatalf("本文を全く送らない POST (ヘッダは完全に送った) が、締め切り (%s) の 20 倍待っても決着しなかった (本文フェーズの Slowloris)", timeout)
	default:
		t.Fatalf("予期しない error: %v", err)
	}
}

// TestServeBothHTTPSlowButProgressingBodyAllowed は、stallGuardBody が、進捗さえ続いていれば、締め切りより
// ずっと長くかかる本文の転送を妨げないことを確かめる (大きい push を、固定の ReadTimeout で打ち切らない、
// という設計の裏付け)。1 バイトずつ、締め切りより短い間隔で送り続けたクライアントの本文が、最後まで
// ハンドラに届くこと。
func TestServeBothHTTPSlowButProgressingBodyAllowed(t *testing.T) {
	const timeout = 80 * time.Millisecond
	connect := New(Config{Audit: io.Discard, HeaderTimeout: timeout, IdleTimeout: timeout})
	l := newTestListener(t)
	const want = "hello"
	got := make(chan string, 1)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("本文の読み取りに失敗: %v", err)
			return
		}
		got <- string(b)
		w.WriteHeader(200)
	})
	go ServeBoth(l, connect, other, "/git/")

	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := fmt.Sprintf("POST /git/x HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", len(want))
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	// 締め切り (80ms) より短い間隔 (20ms) で、1 バイトずつ送る。全体では締め切りの数倍かかる。
	for _, b := range []byte(want) {
		time.Sleep(timeout / 4)
		if _, err := c.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case s := <-got:
		if s != want {
			t.Fatalf("本文 = %q, want %q", s, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("進捗が続いているのに、ハンドラに本文が届かなかった (stall 判定が厳しすぎる)")
	}
}

// TestServeBothHTTPBodyTrickleEventuallyCutOff は、攻撃者視点レビュー (attack-review-40d9745) の再現テスト
// (B3)。stallGuardBody は「読むたびに締め切りを延ばす」進捗ベースの検知だけでは、締め切りより短い間隔で
// 1 バイトずつ送り続けるトリクル (実効スループットがほぼ 0) を原理的に防げない。maxBodyReadDuration が
// 決める絶対の締め切り (Config.MaxBodyBytes から逆算。一度決めたら延長しない) が、これを主として防ぐ。
//
// この絶対の締め切りは、実運用の既定 (512 MiB・64 KiB/s ≒ 2.3 時間) では、このテストのタイムスケールに
// 収まらないため、Config.MaxBodyBytes を小さく指定して、絶対の締め切りを短くする (minBodyThroughput は
// package 内部の定数なので、期待する締め切りをそこから逆算し、マジックナンバーにしない)。
func TestServeBothHTTPBodyTrickleEventuallyCutOff(t *testing.T) {
	const idleTimeout = 20 * time.Millisecond
	const maxBodyBytes = 8 << 10 // 8 KiB
	wantAbsolute := time.Duration(maxBodyBytes) * time.Second / minBodyThroughput
	if wantAbsolute <= idleTimeout {
		t.Fatalf("テストの前提が崩れている: 絶対の締め切り (%s) が IdleTimeout (%s) の floor 以下になっている", wantAbsolute, idleTimeout)
	}
	connect := New(Config{Audit: io.Discard, HeaderTimeout: idleTimeout, IdleTimeout: idleTimeout, MaxBodyBytes: maxBodyBytes})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // Content-Length まで読もうとし続ける (実際には届かない)
		w.WriteHeader(200)
	})
	go ServeBoth(l, connect, other, "/git/")

	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := "POST /git/x HTTP/1.1\r\nHost: x\r\nContent-Length: 1000000\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	// 絶対の締め切りの 4 倍にわたって、IdleTimeout より短い間隔で 1 バイトずつ送り続ける (進捗ベースの
	// stall 検知には、常に「たった今読めた」状態を保たせる)。絶対の締め切りに達したら切られるはず。
	interval := idleTimeout / 2
	deadline := time.Now().Add(4 * wantAbsolute)
	cutOff := false
	for time.Now().Before(deadline) {
		if _, err := c.Write([]byte{'x'}); err != nil {
			cutOff = true // 書き込みが失敗した = サーバに切られた
			break
		}
		time.Sleep(interval)
	}
	if !cutOff {
		// 書き込み側では切られたと分からなかった場合、読み側 (応答 or EOF) でも確認する。
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 16)
		n, err := c.Read(buf)
		switch {
		case err == io.EOF:
			cutOff = true
		case err == nil && n > 0 && strings.HasPrefix(string(buf[:n]), "HTTP/1.1 4"):
			cutOff = true
		}
	}
	if !cutOff {
		t.Fatalf("実効スループットがほぼゼロの接続が、絶対の締め切り (%s) の 4 倍待っても切られなかった (トリクルによる stall guard の回避)", wantAbsolute)
	}
}

// TestServeBothOtherSharesMaxConns は、git/PR 経路の同時接続数が、connect.sem (CONNECT と共有の MaxConns)
// で頭打ちになり、枠を超えた分は 503 で断られることを確かめる。
func TestServeBothOtherSharesMaxConns(t *testing.T) {
	connect := New(Config{Audit: io.Discard, MaxConns: 1})
	l := newTestListener(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release // 1 本目を、明示的に離すまで居座らせる
		w.WriteHeader(200)
	})
	go ServeBoth(l, connect, other, "/git/")

	// 1 本目: ハンドラの中に入ったまま止める (枠を 1 つ使い切る)。Connection: close を付け、離した後は
	// net/http 自身がすぐに閉じるようにする (keep-alive の IdleTimeout 待ちにしない)。
	c1, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	if _, err := c1.Write([]byte("GET /git/x HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("1 本目のハンドラが呼ばれなかった")
	}

	// 2 本目: 枠が無いので、ハンドラを呼ばずに 503 で断られるはず。
	status := dialLine(t, l.Addr().String(), "GET /git/y HTTP/1.1\r\nHost: x\r\nContent-Length: 0")
	if !strings.HasPrefix(status, "HTTP/1.1 503") {
		t.Fatalf("2 本目の status = %q, want 503", status)
	}

	// 1 本目を離す。枠が返る (ConnState の StateClosed) のは、応答を書き終えて接続が閉じた後、非同期に
	// 起きるので、3 本目は少し再試行しながら待つ。
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		status = dialLine(t, l.Addr().String(), "GET /git/z HTTP/1.1\r\nHost: x\r\nContent-Length: 0")
		if strings.HasPrefix(status, "HTTP/1.1 200") {
			break
		}
		if !strings.HasPrefix(status, "HTTP/1.1 503") || time.Now().After(deadline) {
			t.Fatalf("3 本目 (枠が返った後) の status = %q, want 200 (503 のまま粘るなら、枠が返っていない)", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestServeBothConcurrent は、CONNECT と git/HTTP の要求を、同じ listener に混ぜて多数投げても、取り違えないことを確かめる。
func TestServeBothConcurrent(t *testing.T) {
	connect := New(Config{Allow: []string{"allowed.invalid:443"}, Audit: io.Discard})
	var hits atomicInt
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.add(1); w.WriteHeader(200) })
	l := newTestListener(t)
	go ServeBoth(l, connect, other, "/git/")

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				status := dialLine(t, l.Addr().String(), "CONNECT not-allowed.invalid:443 HTTP/1.1")
				if !strings.HasPrefix(status, "HTTP/1.1 403") {
					t.Errorf("connect: status = %q", status)
				}
			} else {
				status := dialLine(t, l.Addr().String(), "GET /git/x HTTP/1.1\r\nHost: x\r\nContent-Length: 0")
				if !strings.HasPrefix(status, "HTTP/1.1 200") {
					t.Errorf("git: status = %q", status)
				}
			}
		}(i)
	}
	wg.Wait()
	if got := hits.get(); got != 20 {
		t.Fatalf("git ハンドラは 20 回呼ばれるはず: %d", got)
	}
}

// FuzzDispatch は、任意の最初の行で、dispatch がパニックしないことを確かめる。
func FuzzDispatch(f *testing.F) {
	for _, s := range []string{"CONNECT x:443 HTTP/1.1", "GET /git/x HTTP/1.1", "GARBAGE", "", "CONNECT", "GET /github-api/x", strings.Repeat("A", 5000)} {
		f.Add(s)
	}
	connect := New(Config{Audit: io.Discard})
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	// newOtherHTTPServer を使う (素の &http.Server{Handler: other} だと ConnState が無く、dispatch が
	// git/PR 経路で取る connect.sem の枠が解放されないまま、既定 128 回で枯渇して以後すべて busy になる)。
	httpSrv := newOtherHTTPServer(connect, other)
	f.Fuzz(func(t *testing.T, line string) {
		server, client := net.Pipe()
		done := make(chan struct{})
		go func() {
			dispatch(server, connect, httpSrv, []string{"/git/", "/github-api/"})
			close(done)
		}()
		client.SetDeadline(time.Now().Add(2 * time.Second))
		client.Write([]byte(line + "\r\n"))
		io.Copy(io.Discard, client)
		client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
}

// writerFunc は、関数を io.Writer にする。
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// atomicInt は、テストだけで使う、単純な排他カウンタ。
type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) add(d int) {
	a.mu.Lock()
	a.n += d
	a.mu.Unlock()
}
func (a *atomicInt) get() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

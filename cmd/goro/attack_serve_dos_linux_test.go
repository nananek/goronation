package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	iwebauthn "github.com/nananek/goronation/cmd/internal/webauthn"
)

// TestServeHTTPBodyStallEnforced は、攻撃者視点レビュー (attack-review-0d1f25a) の再現テスト。
//
// runServeServer (serve.go) が実際に構築する *http.Server は、当初 ReadHeaderTimeout だけを設定して
// おり、ReadTimeout・WriteTimeout・IdleTimeout・同時接続数の上限のいずれも無かった。egress/gateway で
// 過去に複数回見つかった Slowloris 系の DoS (B1〜B4: PR #29 の attack-review-bc6d03a 以降) と同じ形の
// 攻撃が、goro serve (初のネットワーク待ち受け daemon) にもそのまま成立していたことを、実際に生の TCP で
// 確かめる (newServeHTTPServer・newLimitedListener で修正済み。このテストは、runServeServer が実際に
// 使うのと同じ組み立てを、直接呼んで確かめる)。
//
// ヘッダは完全かつ即座に送るが、宣言した Content-Length の本文を全く送らないクライアントに対して、
// このテストは「妥当な時間内に、何らかの決着 (切断か、エラー応答) が付くべき」という期待を書く。
func TestServeHTTPBodyStallEnforced(t *testing.T) {
	dir := t.TempDir()
	store, err := iwebauthn.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + l.Addr().String()
	cfg := iwebauthn.Config{RPID: "127.0.0.1", RPName: "test", Origin: origin}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// runServeServer (serve.go) と、まったく同じ *http.Server・Listener の組み立て。
	srv := newServeHTTPServer(newServeMux(cfg, store, origin, nil))
	go srv.Serve(newLimitedListener(l, maxServeConns))
	t.Cleanup(func() { srv.Close() })

	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// ヘッダは完全に・即座に送る (認証前に叩けるエンドポイントに、Content-Length: 100000 を宣言するが、
	// 本文は 1 バイトも送らない)。
	req := "POST /webauthn/register/begin HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	// serveReadTimeout (30秒) より長い、妥当な猶予の間に、何らかの決着 (切断か、エラー応答) が付くはず。
	c.SetReadDeadline(time.Now().Add(serveReadTimeout + 15*time.Second))
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	switch {
	case err == nil:
		if n == 0 {
			t.Fatal("0 バイトの応答")
		}
		return // 何らかの応答 (400/408 など) が来れば OK
	case err == io.EOF:
		return // サーバーが接続を閉じた: 期待どおり
	case os.IsTimeout(err):
		t.Fatal("本文を全く送らない POST (ヘッダは完全に送った) が、ReadTimeout を超えても決着しなかった (goro serve は初のネットワーク待ち受け daemon であり、認証前のエンドポイントに対する Slowloris 耐性が無い)")
	default:
		t.Fatalf("予期しない error: %v", err)
	}
}

// TestServeManyStalledConnectionsAccepted は、goro serve の *http.Server (と、その手前の net.Listener)
// に、同時接続数の上限に相当する仕組みが無いことを、実際に多数の「ヘッダのみ・本文を送らない」接続を張って
// 確かめる (情報の記録用。attack-review-0d1f25a の指摘どおり、この結果自体は Blocking ではない)。
//
// newLimitedListener は、http.Server.Serve が Accept を呼ぶ回数 (=実際に読み取り処理へ進める接続の数)
// を絞るだけで、TCP の 3-way handshake 自体 (kernel の accept queue に積まれること) は妨げない。この
// テストが「300 本とも、生の TCP としては受理される」ことを確認するのは、その kernel レベルの話であり、
// goro serve 自身の同時接続数の上限 (limitedListener が絞る、実際に処理される接続の数) の効果は、
// 別の TestLimitedListenerCapsAccepts で直接確認する。
func TestServeManyStalledConnectionsAccepted(t *testing.T) {
	dir := t.TempDir()
	store, err := iwebauthn.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + l.Addr().String()
	cfg := iwebauthn.Config{RPID: "127.0.0.1", RPName: "test", Origin: origin}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv := newServeHTTPServer(newServeMux(cfg, store, origin, nil))
	go srv.Serve(newLimitedListener(l, maxServeConns))
	t.Cleanup(func() { srv.Close() })

	const n = 300 // 一般的な妥当な同時接続数の上限を上回る数
	conns := make([]net.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	req := "POST /webauthn/register/begin HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n"
	accepted := 0
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
		if err != nil {
			break
		}
		if _, err := c.Write([]byte(req)); err != nil {
			c.Close()
			break
		}
		conns = append(conns, c)
		accepted++
	}
	if accepted < n {
		t.Skipf("この検証環境の制約で %d/%d 本しか開けなかった (サーバー自身が断った形跡は無い)", accepted, n)
	}
	t.Logf("%d 本の「ヘッダのみ・本文を送らない」生の TCP 接続が、kernel レベルでは一切拒否されずに受理された", accepted)
}

// TestLimitedListenerCapsAccepts は、limitedListener (serve.go) が、実際に Accept を呼ぶ回数 (=処理へ
// 進める接続の数) を max に絞り、超えた分は、既存の接続が閉じるまで Accept 自体が進まないことを、
// net.Listener のレベルで直接確かめる (goro serve の同時接続数の上限の、本体の検証)。
func TestLimitedListenerCapsAccepts(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	const max = 3
	ll := newLimitedListener(raw, max)

	accepted := make(chan net.Conn, max+1)
	go func() {
		for {
			c, err := ll.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	dial := func() net.Conn {
		c, err := net.DialTimeout("tcp", raw.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// max 本までは、素直に accept される。空きを 1 つ空けるのに使う、サーバー側 (Accept が返した方) の
	// 接続を、別に持っておく (クライアント側を閉じても、limitedListener の枠は空かない: 空くのは、
	// Accept が返した limitedConn 自身が Close されたときだけ)。
	var serverSide0 net.Conn
	for i := 0; i < max; i++ {
		c := dial()
		t.Cleanup(func() { c.Close() })
		select {
		case c := <-accepted:
			if i == 0 {
				serverSide0 = c
			} else {
				t.Cleanup(func() { c.Close() })
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%d 本目が accept されない (max=%d 以内のはず)", i+1, max)
		}
	}

	// max+1 本目は、TCP の handshake 自体は通っても (kernel の accept queue に積まれる)、
	// limitedListener の Accept は、空きが無いので進まない。
	extra := dial()
	t.Cleanup(func() { extra.Close() })
	select {
	case <-accepted:
		t.Fatal("max を超えた接続が、空きが無いのに accept された")
	case <-time.After(200 * time.Millisecond):
		// 期待どおり: 空きが無い間、accept が進まない。
	}

	// 既存の 1 本 (サーバー側) を閉じると、枠が空いて、max+1 本目が accept される。
	serverSide0.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("空きができたのに、待っていた接続が accept されない")
	}
}

// TestServeKeepAliveDisabledBoundsConnectionHoldTime は、攻撃者視点レビュー (attack-review-28d386c
// への OpenCode による独立レビューと、それを検証した増分レビュー) が見つけた可用性 DoS の再現・修正確認。
//
// limitedListener の枠は、接続が Close されるまで解放されない。HTTP keep-alive では、応答を返し終えた
// 接続も Close されず、IdleTimeout (60秒の無通信) まで枠を保持し続ける。IdleTimeout は無通信の締め切りに
// 過ぎないので、64 本の keep-alive 接続を IdleTimeout の内側で (例えば 45〜60 秒ごとに 1 リクエスト)
// 更新し続けるだけで、枠を無期限に占有でき、65 本目 (正規の利用者) が永久に応答されない、という可用性
// DoS が、newServeHTTPServer 修正当初の版 (ReadTimeout/WriteTimeout/IdleTimeout・同時接続数の上限だけ)
// には残っていた (body stall ではなく、完全で正当なリクエストを繰り返すだけで再現するため、ReadTimeout も
// 無関係だった)。
//
// newServeHTTPServer が SetKeepAlivesEnabled(false) を呼ぶようになった今は、1 接続=最大 1 リクエストに
// 強制される (応答後、サーバー側が接続を閉じ、枠を解放する) ので、この具体的な攻撃 (同一接続への周期的な
// 延命) は構造的に閉じている。ただし、可用性 DoS の完全な根治ではない: body をゆっくり送る (ReadTimeout
// いっぱいまで) → 都度接続を張り直す、という、攻撃コストが上がった変種は残る (doc.go の「限界」を参照)。
func TestServeKeepAliveDisabledBoundsConnectionHoldTime(t *testing.T) {
	dir := t.TempDir()
	store, err := iwebauthn.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + l.Addr().String()
	cfg := iwebauthn.Config{RPID: "127.0.0.1", RPName: "test", Origin: origin}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv := newServeHTTPServer(newServeMux(cfg, store, origin, nil))
	go srv.Serve(newLimitedListener(l, maxServeConns))
	t.Cleanup(func() { srv.Close() })

	getReq := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	dial := func() net.Conn {
		c, err := net.DialTimeout("tcp", l.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// 1) 攻撃者のつもりで、max 本を「保持する」ために GET / を送って応答を読む。keep-alive が無効なら、
	// クライアントが接続を閉じなくても、応答のたびにサーバー側が閉じる (Connection: close が付く)。
	conns := make([]net.Conn, 0, maxServeConns)
	for i := 0; i < maxServeConns; i++ {
		c := dial()
		t.Cleanup(func() { c.Close() })
		if _, err := c.Write(getReq); err != nil {
			t.Fatalf("%d 本目の要求を送れない: %v", i+1, err)
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "GET"})
		if err != nil {
			t.Fatalf("%d 本目の応答を読めない: %v", i+1, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// http.ReadResponse は Connection ヘッダ自体は取り除くが (hop-by-hop)、resp.Close に反映する。
		if !resp.Close {
			t.Errorf("%d 本目: resp.Close = false (keep-alive が無効になっていない)", i+1)
		}
		conns = append(conns, c)
	}

	// 2) 同じ接続への 2 回目のリクエストは通らない (サーバーが 1 回目の応答後にすでに閉じているはず)。
	conns[0].SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conns[0].Write(getReq); err == nil {
		if _, err := bufio.NewReader(conns[0]).ReadByte(); err == nil {
			t.Error("keep-alive が無効なはずなのに、同じ接続への 2 回目のリクエストが通った (枠を保持し続けられる)")
		}
	}

	// 3) 65 本目 (正規の利用者) は、待たされずにすぐ処理される: 攻撃者が「保持したつもり」の 64 本は、
	// 応答した時点でサーバー側からすでに閉じられていて、枠は解放済みのはず。
	extra := dial()
	t.Cleanup(func() { extra.Close() })
	if _, err := extra.Write(getReq); err != nil {
		t.Fatal(err)
	}
	extra.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(extra), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("65 本目がすぐ処理されなかった (枠が解放されていない・可用性 DoS が再現している): %v", err)
	}
	resp.Body.Close()
}

package connectproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// echoListener は、繋いできた接続に、受け取ったバイトをそのまま返す (中継の両方向を確かめるための、
// テスト用の上流)。
func echoListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return l
}

func startProxy(t *testing.T, allow map[string]netip.AddrPort) (addr string, srv *Server) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv = New(allow)
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String(), srv
}

func dial(t *testing.T, proxyAddr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func sendConnect(t *testing.T, c net.Conn, target string) *bufio.Reader {
	t.Helper()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	return bufio.NewReader(c)
}

func readStatusLine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func TestConnectAllowedRelaysBothDirections(t *testing.T) {
	up := echoListener(t)
	real := netip.MustParseAddrPort(up.Addr().String())
	proxyAddr, _ := startProxy(t, map[string]netip.AddrPort{"fake-openai.test:443": real})

	c := dial(t, proxyAddr)
	br := sendConnect(t, c, "fake-openai.test:443")
	status := readStatusLine(t, br)
	if status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("status = %q", status)
	}
	// 200 の後の空行 (ヘッダー無し) を読み飛ばす。
	if line, _ := br.ReadString('\n'); line != "\r\n" {
		t.Fatalf("200 の後の空行 = %q", line)
	}

	msg := []byte("hello through the tunnel")
	if _, err := c.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n := 0
	for n < len(msg) {
		m, err := br.Read(got[n:])
		if err != nil {
			t.Fatalf("read echo: %v", err)
		}
		n += m
	}
	if string(got) != string(msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

func TestConnectNotAllowedIs403(t *testing.T) {
	proxyAddr, _ := startProxy(t, map[string]netip.AddrPort{
		"fake-openai.test:443": netip.MustParseAddrPort("127.0.0.1:1"),
	})
	c := dial(t, proxyAddr)
	br := sendConnect(t, c, "evil.example.com:443")
	status := readStatusLine(t, br)
	if status != "HTTP/1.1 403 Forbidden" {
		t.Fatalf("status = %q, want 403", status)
	}
}

func TestNonConnectMethodIs405(t *testing.T) {
	proxyAddr, _ := startProxy(t, map[string]netip.AddrPort{
		"fake-openai.test:443": netip.MustParseAddrPort("127.0.0.1:1"),
	})
	c := dial(t, proxyAddr)
	fmt.Fprintf(c, "GET http://fake-openai.test/v1/chat/completions HTTP/1.1\r\nHost: fake-openai.test\r\n\r\n")
	br := bufio.NewReader(c)
	status := readStatusLine(t, br)
	if status != "HTTP/1.1 405 Method Not Allowed" {
		t.Fatalf("status = %q, want 405", status)
	}
}

func TestCaseInsensitiveHost(t *testing.T) {
	up := echoListener(t)
	real := netip.MustParseAddrPort(up.Addr().String())
	proxyAddr, _ := startProxy(t, map[string]netip.AddrPort{"Fake-OpenAI.test:443": real})

	c := dial(t, proxyAddr)
	br := sendConnect(t, c, "FAKE-openai.TEST:443")
	status := readStatusLine(t, br)
	if status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("status = %q, want 200 (大文字小文字を区別しないはず)", status)
	}
}

func TestDialFailureIs502(t *testing.T) {
	// 実際には何も listen していないポートを、許可表の実アドレスにする (dial が確実に失敗する)。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := netip.MustParseAddrPort(l.Addr().String())
	l.Close() // すぐ閉じる: このポートへの接続は、確実に失敗する

	proxyAddr, _ := startProxy(t, map[string]netip.AddrPort{"fake-openai.test:443": closedAddr})
	c := dial(t, proxyAddr)
	br := sendConnect(t, c, "fake-openai.test:443")
	status := readStatusLine(t, br)
	if status != "HTTP/1.1 502 Bad Gateway" {
		t.Fatalf("status = %q, want 502", status)
	}
}

func TestMalformedRequestLineIs400(t *testing.T) {
	proxyAddr, _ := startProxy(t, map[string]netip.AddrPort{
		"fake-openai.test:443": netip.MustParseAddrPort("127.0.0.1:1"),
	})
	c := dial(t, proxyAddr)
	fmt.Fprint(c, "not even http\r\n\r\n")
	br := bufio.NewReader(c)
	status := readStatusLine(t, br)
	if status != "HTTP/1.1 400 Bad Request" {
		t.Fatalf("status = %q, want 400", status)
	}
}

// TestUnterminatedLineClosesWithoutHanging は、CRLF で終わらない行 (相手が接続を閉じただけ) で、
// panic もハングもせず、単に閉じることを確かめる (readLine が error を返す経路。応答は書けない
// のでそのまま閉じるのが正しい)。
func TestUnterminatedLineClosesWithoutHanging(t *testing.T) {
	proxyAddr, _ := startProxy(t, map[string]netip.AddrPort{
		"fake-openai.test:443": netip.MustParseAddrPort("127.0.0.1:1"),
	})
	c := dial(t, proxyAddr)
	fmt.Fprint(c, "CONNECT fake-openai.test:443 HTTP/1.1")
	c.(*net.TCPConn).CloseWrite() // 書き込み側だけ閉じる: 相手は「行の途中で EOF」を見る
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatalf("CRLF で終わらない行を送ったのに、何か読めてしまった (応答を書くべきではない)")
	}
}

// TestSlowHeaderTimesOut は、CONNECT 行を送り切らずに接続だけ繋ぎ続ける接続が、HeaderTimeout を
// 過ぎても無期限にハングしない (tools/fakeproviders の finding 2/4 と同種の教訓の回帰確認) ことを
// 確かめる。
func TestSlowHeaderTimesOut(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(map[string]netip.AddrPort{"fake-openai.test:443": netip.MustParseAddrPort("127.0.0.1:1")})
	srv.HeaderTimeout = 100 * time.Millisecond
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	c := dial(t, l.Addr().String())
	fmt.Fprint(c, "CONNECT fake-openai.test:443 HTTP/1.1\r\nHost: ") // ヘッダー終端の空行を送らない

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		c.Read(buf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ヘッダーを送り切らない接続が HeaderTimeout を過ぎてもハングし続けた")
	}
}

func TestCloseStopsServeAndActiveConns(t *testing.T) {
	up := echoListener(t)
	real := netip.MustParseAddrPort(up.Addr().String())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(map[string]netip.AddrPort{"fake-openai.test:443": real})
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()

	c := dial(t, l.Addr().String())
	br := sendConnect(t, c, "fake-openai.test:443")
	if status := readStatusLine(t, br); status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("status = %q", status)
	}

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve() 戻り値 = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 後も Serve が戻らない")
	}

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("Close 後も、確立済みの接続が閉じられていない")
	}
}

func TestNewLowercasesAllowKeys(t *testing.T) {
	real := netip.MustParseAddrPort("127.0.0.1:1")
	srv := New(map[string]netip.AddrPort{"Mixed-Case.test:443": real})
	if _, ok := srv.allow["mixed-case.test:443"]; !ok {
		t.Fatalf("allow の key が小文字化されていない: %+v", srv.allow)
	}
}

// dialOverrideForTest は、DialFailureIs502 以外の経路でも dial を差し替えられることを確認するための、
// context キャンセルへの追従テスト。
func TestDialRespectsContextCancellation(t *testing.T) {
	proxyAddr, srv := startProxy(t, map[string]netip.AddrPort{
		"fake-openai.test:443": netip.MustParseAddrPort("127.0.0.1:1"),
	})
	srv.DialTimeout = 50 * time.Millisecond
	srv.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c := dial(t, proxyAddr)
	br := sendConnect(t, c, "fake-openai.test:443")
	status := readStatusLine(t, br)
	if status != "HTTP/1.1 502 Bad Gateway" {
		t.Fatalf("status = %q, want 502 (DialTimeout で打ち切られるはず)", status)
	}
}

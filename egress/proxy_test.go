package egress

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRelay は、許可した宛先で、バイト列が両方向に、欠けず往復することを確かめる。
func TestRelay(t *testing.T) {
	u := newUpstream(t, echo)
	p := forUpstream(t, u, nil)
	c, br := p.connect(t, u.target())

	payload := randomBytes(t, 1<<20)
	go func() {
		c.Write(payload)
		c.CloseWrite() // 半クローズ: 上流の echo が EOF を見て、書き込み側を閉じる
	}()
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("往復したバイト列が一致しない: %d バイト (送った %d バイト)", len(got), len(payload))
	}

	r := p.audit.only(t)
	if r.Event != "allow" || r.Reason != reasonAllowlist || r.Target != u.target() || r.IP != "127.0.0.1" || r.Status != 200 {
		t.Errorf("監査 = %+v", r)
	}
	if n := u.connections(); n != 1 {
		t.Errorf("上流への接続 = %d, want 1", n)
	}
}

// TestRelayNameCaseInsensitive は、名前の大文字小文字を区別せず、監査には正規化した宛先が出ることを固定する。
func TestRelayNameCaseInsensitive(t *testing.T) {
	u := newUpstream(t, echo)
	p := forUpstream(t, u, nil)
	c, br := p.connect(t, strings.ToUpper(u.target()))
	io.WriteString(c, "x")
	b := make([]byte, 1)
	if _, err := io.ReadFull(br, b); err != nil || b[0] != 'x' {
		t.Fatalf("echo = %q, %v", b, err)
	}
	if r := p.audit.only(t); r.Target != u.target() {
		t.Errorf("監査の target = %q, want %q", r.Target, u.target())
	}
}

// TestRelayPipelinedBytes は、200 を待たずに、ヘッダの直後に送られたバイトも、上流に届くことを固定する。
func TestRelayPipelinedBytes(t *testing.T) {
	got := make(chan []byte, 1)
	u := newUpstream(t, func(c net.Conn) {
		b := make([]byte, 5)
		io.ReadFull(c, b)
		got <- b
	})
	p := forUpstream(t, u, nil)
	c, br := p.open(t)
	io.WriteString(c, connectRequest(u.target())+"EARLY")
	if r, err := readHead(br); err != nil || r.code != 200 {
		t.Fatalf("応答 = %+v, %v", r, err)
	}
	select {
	case b := <-got:
		if string(b) != "EARLY" {
			t.Errorf("上流が受けたバイト = %q, want EARLY", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("先送りしたバイトが、上流に届かない")
	}
}

// TestHalfCloseFromUpstream は、上流が先に書き込みを閉じても、client → 上流の向きが続くことを固定する。
func TestHalfCloseFromUpstream(t *testing.T) {
	late := make(chan string, 1)
	u := newUpstream(t, func(c net.Conn) {
		io.WriteString(c, "hello")
		c.(*net.TCPConn).CloseWrite()
		b, _ := io.ReadAll(c) // client が閉じるまで読む
		late <- string(b)
	})
	p := forUpstream(t, u, nil)
	c, br := p.connect(t, u.target())

	b := make([]byte, 5)
	if _, err := io.ReadFull(br, b); err != nil || string(b) != "hello" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if _, err := br.ReadByte(); err != io.EOF { // 上流の半クローズが、client の EOF になる
		t.Fatalf("上流が閉じた後の read = %v, want EOF", err)
	}
	io.WriteString(c, "late") // client 側は、まだ書ける
	c.CloseWrite()
	select {
	case s := <-late:
		if s != "late" {
			t.Errorf("上流が受けたバイト = %q, want late", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("半クローズの後の client → 上流が、届かない")
	}
}

// TestUpstreamCloseEndsTunnel は、上流が接続を閉じたら、client が EOF を見ることを固定する。
func TestUpstreamCloseEndsTunnel(t *testing.T) {
	u := newUpstream(t, func(c net.Conn) { io.WriteString(c, "bye") })
	p := forUpstream(t, u, nil)
	_, br := p.connect(t, u.target())
	got, err := io.ReadAll(br)
	if err != nil || string(got) != "bye" {
		t.Errorf("ReadAll = %q, %v", got, err)
	}
}

// TestConcurrentRelay は、多数の接続が同時に、互いのバイト列を混ぜずに中継されることを固定する (-race で回す)。
func TestConcurrentRelay(t *testing.T) {
	u := newUpstream(t, echo)
	p := forUpstream(t, u, func(c *Config) { c.MaxConns = 64 })
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := randomBytes(t, 64<<10)
			r, c, br := p.request(t, connectRequest(u.target()))
			if r.code != 200 {
				errs <- fmt.Errorf("状態コード = %d", r.code)
				return
			}
			go func() {
				c.Write(payload)
				c.(*net.UnixConn).CloseWrite()
			}()
			got, err := io.ReadAll(br)
			if err != nil || !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("往復が一致しない: %d バイト, %v", len(got), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(p.audit.records(t)); got != n {
		t.Errorf("監査 = %d 行, want %d", got, n)
	}
}

// TestMaxConns は、同時接続数の上限 (ヘッダを読んでいる間も数える) と、枠の解放を固定する。
func TestMaxConns(t *testing.T) {
	u := newUpstream(t, echo)
	p := forUpstream(t, u, func(c *Config) { c.MaxConns = 2 })

	// 1 本は、トンネルで塞ぐ。もう 1 本は、何も送らずに (ヘッダを読ませたまま) 塞ぐ。
	tunnel, _ := p.connect(t, u.target())
	idle, _ := p.open(t)

	r, c, br := p.requestLenient(t, connectRequest(u.target()))
	if r.code != 503 {
		t.Fatalf("上限を超えた接続の状態コード = %d, want 503", r.code)
	}
	if _, err := br.ReadByte(); err == nil {
		t.Error("503 の後も、接続が開いている")
	}
	c.Close()
	recs := p.audit.records(t)
	if last := recs[len(recs)-1]; last.Event != "deny" || last.Reason != reasonBusy || last.Status != 503 {
		t.Errorf("監査 = %+v", last)
	}

	// 枠が空けば、また受ける。
	tunnel.Close()
	eventually(t, "枠が解放される", func() bool {
		r, c, _ := p.requestLenient(t, connectRequest(u.target()))
		defer c.Close()
		return r.code == 200
	})
	idle.Close()
	if n := u.connections(); n < 2 {
		t.Errorf("上流への接続 = %d, want 2 以上", n)
	}
}

// TestIdleTimeout は、両方向の無通信を期限で切ること、どちらかが動いている間は切らないことを固定する。
func TestIdleTimeout(t *testing.T) {
	const idle = 300 * time.Millisecond

	t.Run("無通信は切る", func(t *testing.T) {
		upClosed := make(chan struct{})
		u := newUpstream(t, func(c net.Conn) { io.Copy(io.Discard, c); close(upClosed) })
		p := forUpstream(t, u, func(c *Config) { c.IdleTimeout = idle })
		_, br := p.connect(t, u.target())
		start := time.Now()
		if _, err := br.ReadByte(); err == nil {
			t.Fatal("無通信なのに、データが読めた")
		}
		if d := time.Since(start); d < idle/2 || d > 5*time.Second {
			t.Errorf("切れるまで %v (期限 %v)", d, idle)
		}
		select {
		case <-upClosed:
		case <-time.After(5 * time.Second):
			t.Error("client 側が切れても、上流側が閉じない")
		}
	})

	t.Run("client → 上流が動いている間は続く", func(t *testing.T) {
		var got bytes.Buffer
		var mu sync.Mutex
		u := newUpstream(t, func(c net.Conn) {
			b := make([]byte, 1)
			for {
				if _, err := c.Read(b); err != nil {
					return
				}
				mu.Lock()
				got.Write(b)
				mu.Unlock()
			}
		})
		p := forUpstream(t, u, func(c *Config) { c.IdleTimeout = idle })
		c, _ := p.connect(t, u.target())
		for i := 0; i < 12; i++ { // 600ms > 期限 (300ms)。1 バイトずつ、50ms おきに送る
			if _, err := c.Write([]byte{'a'}); err != nil {
				t.Fatalf("%d 回目の write: %v (動いているのに切れた)", i, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		eventually(t, "12 バイトが届く", func() bool { mu.Lock(); defer mu.Unlock(); return got.Len() == 12 })
	})

	t.Run("上流 → client だけが動いていても続く", func(t *testing.T) {
		u := newUpstream(t, func(c net.Conn) {
			for i := 0; i < 12; i++ {
				if _, err := c.Write([]byte{'b'}); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
		p := forUpstream(t, u, func(c *Config) { c.IdleTimeout = idle })
		_, br := p.connect(t, u.target())
		got, err := io.ReadAll(br) // 上流が 12 バイト送って閉じるまで
		if err != nil || string(got) != strings.Repeat("b", 12) {
			t.Errorf("ReadAll = %q, %v (client が黙っていても、上流が動いている間は切らない)", got, err)
		}
	})
}

// TestClose は、Close が、待ち受け・トンネル・ヘッダ待ちの接続をすべて閉じ、終わるのを待つことを固定する。
func TestClose(t *testing.T) {
	upClosed := make(chan struct{})
	u := newUpstream(t, func(c net.Conn) { io.Copy(io.Discard, c); close(upClosed) })
	p := forUpstream(t, u, nil)
	_, tunnel := p.connect(t, u.target())
	_, waiting := p.open(t) // ヘッダを待たせたままにする

	closed := make(chan error, 1)
	go func() { closed <- p.s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close が返らない")
	}
	err := <-p.done
	p.done <- err // cleanup も待つので、戻す
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Serve = %v, want ErrClosed", err)
	}
	if _, err := tunnel.ReadByte(); err == nil {
		t.Error("Close の後も、トンネルが開いている")
	}
	if _, err := waiting.ReadByte(); err == nil {
		t.Error("Close の後も、ヘッダ待ちの接続が開いている")
	}
	select {
	case <-upClosed:
	case <-time.After(5 * time.Second):
		t.Error("Close の後も、上流への接続が開いている")
	}
	if c, err := net.Dial("unix", p.path); err == nil {
		c.Close()
		t.Error("Close の後も、待ち受けている")
	}
	if err := p.s.Close(); err != nil { // 2 回目
		t.Errorf("2 回目の Close = %v", err)
	}
}

// serve は、s.Serve(l) を回す。2 秒以内に返らなければ、Close して失敗にする (返らない実装で、テストが固まらないように)。
func serve(t *testing.T, s *Server, l net.Listener) error {
	t.Helper()
	t.Cleanup(func() { l.Close() })
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		s.Close()
		t.Fatal("Serve が返らない")
		return nil
	}
}

// assertClosed は、l が閉じられている (もう接続を受けない) ことを確かめる。
func assertClosed(t *testing.T, l net.Listener) {
	t.Helper()
	if c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Error("Serve が返った後も、l が開いている")
	}
}

// TestServeAfterClose は、Close した Server の Serve が、ErrClosed を返して、l を閉じることを固定する。
func TestServeAfterClose(t *testing.T) {
	s := New(Config{Audit: io.Discard})
	s.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(t, s, l); !errors.Is(err, ErrClosed) {
		t.Errorf("Serve = %v, want ErrClosed", err)
	}
	assertClosed(t, l)
}

// TestServeReturnsWhenListenerClosed は、l が外から閉じられたら、Serve が (ErrClosed 以外の) error で返ることを固定する。
func TestServeReturnsWhenListenerClosed(t *testing.T) {
	s := New(Config{Audit: io.Discard})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	time.Sleep(20 * time.Millisecond)
	l.Close()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrClosed) {
			t.Errorf("Serve = %v, want listener の error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve が返らない")
	}
	s.Close()
}

// TestConfig は、不正な Config が、Serve の ErrConfig になることと、正しい Config が通ることを固定する。
func TestConfig(t *testing.T) {
	bad := []struct {
		name string
		cfg  Config
	}{
		{"Audit が nil", Config{Allow: ClaudeHosts()}},
		{"IPv4 リテラル", Config{Allow: []string{"127.0.0.1:443"}, Audit: io.Discard}},
		{"IPv6 リテラル", Config{Allow: []string{"[::1]:443"}, Audit: io.Discard}},
		{"ポートが無い", Config{Allow: []string{"api.anthropic.com"}, Audit: io.Discard}},
		{"ワイルドカード", Config{Allow: []string{"*.anthropic.com:443"}, Audit: io.Discard}},
		{"userinfo", Config{Allow: []string{"user@api.anthropic.com:443"}, Audit: io.Discard}},
		{"ポート 0", Config{Allow: []string{"api.anthropic.com:0"}, Audit: io.Discard}},
		{"ポートが範囲外", Config{Allow: []string{"api.anthropic.com:65536"}, Audit: io.Discard}},
		{"1 つでも不正", Config{Allow: []string{"api.anthropic.com:443", "10.0.0.1:443"}, Audit: io.Discard}},
		{"MaxConns が負", Config{Audit: io.Discard, MaxConns: -1}},
		{"MaxHeaderBytes が負", Config{Audit: io.Discard, MaxHeaderBytes: -1}},
		{"HeaderTimeout が負", Config{Audit: io.Discard, HeaderTimeout: -1}},
		{"DialTimeout が負", Config{Audit: io.Discard, DialTimeout: -1}},
		{"IdleTimeout が負", Config{Audit: io.Discard, IdleTimeout: -1}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			if err := serve(t, New(tt.cfg), l); !errors.Is(err, ErrConfig) {
				t.Errorf("Serve = %v, want ErrConfig", err)
			}
			assertClosed(t, l)
		})
	}

	s := New(Config{Allow: []string{"API.Anthropic.com:443", "a.example:1"}, Audit: io.Discard})
	if s.err != nil {
		t.Fatalf("正しい Config が通らない: %v", s.err)
	}
	if _, ok := s.allow["api.anthropic.com:443"]; !ok {
		t.Errorf("許可の表 = %v (小文字に正規化されるはず)", s.allow)
	}
	if s := New(Config{Audit: io.Discard}); s.err != nil { // 空の Allow は、すべて拒否
		t.Errorf("空の Allow が通らない: %v", s.err)
	}
}

// TestAuditFailure は、監査に書けないとき、許可を出さない (fail-closed) こと、拒否は拒否のままであることを固定する。
func TestAuditFailure(t *testing.T) {
	u := newUpstream(t, echo)
	p := forUpstream(t, u, nil)
	p.audit.fail.Store(true)

	r, c, br := p.request(t, connectRequest(u.target()))
	if r.code != 503 {
		t.Errorf("監査に書けないときの状態コード = %d, want 503", r.code)
	}
	if _, err := br.ReadByte(); err == nil {
		t.Error("監査に書けないのに、トンネルが開いている")
	}
	c.Close()

	if r, _, _ := p.request(t, "GET / HTTP/1.1\r\n\r\n"); r.code != 405 {
		t.Errorf("拒否の状態コード = %d, want 405", r.code)
	}

	p.audit.fail.Store(false)
	if r, _, _ := p.request(t, connectRequest(u.target())); r.code != 200 {
		t.Errorf("監査が直った後の状態コード = %d, want 200", r.code)
	}
}

// TestAuditLines は、監査が 1 行 1 JSON で、必須のフィールドを持ち、ヘッダの値や秘密を含まないことを固定する。
func TestAuditLines(t *testing.T) {
	u := newUpstream(t, echo)
	p := forUpstream(t, u, nil)
	bidi := string(rune(0x202e))
	reqs := []string{
		"CONNECT " + u.target() + " HTTP/1.1\r\nProxy-Authorization: Basic SECRET-HEADER\r\nHost: SECRET-HOST\r\n\r\n",
		"CONNECT user:SECRET-USERINFO@" + u.target() + " HTTP/1.1\r\n\r\n",
		"CONNECT evil" + bidi + ".example:443 HTTP/1.1\r\nX-Token: SECRET-TOKEN\r\n\r\n",
		"GET http://example.com/?token=SECRET-QUERY HTTP/1.1\r\nCookie: SECRET-COOKIE\r\n\r\n",
		"CONNECT 127.0.0.1:1 HTTP/1.1\r\n\r\n",
	}
	for _, r := range reqs {
		_, c, _ := p.request(t, r)
		c.Close()
	}
	out := p.audit.String()
	if strings.Contains(out, "SECRET") {
		t.Errorf("監査に秘密が出ている:\n%s", out)
	}
	if strings.Contains(out, bidi) {
		t.Errorf("監査に双方向制御文字が出ている:\n%q", out)
	}
	recs := p.audit.records(t)
	if len(recs) != len(reqs) {
		t.Fatalf("監査 = %d 行, want %d", len(recs), len(reqs))
	}
	var conns []uint64
	for _, r := range recs {
		if r.Event == "" || r.Reason == "" || r.Status == 0 || r.Conn == 0 {
			t.Errorf("必須のフィールドが空: %+v", r)
		}
		conns = append(conns, r.Conn)
	}
	slices.Sort(conns)
	if len(slices.Compact(conns)) != len(conns) {
		t.Errorf("接続の番号が重複している: %v", conns)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.ContainsAny(line, "\r\x00") {
			t.Errorf("監査の行に制御文字: %q", line)
		}
	}
}

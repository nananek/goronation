package egress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubResolver は、テスト用の Resolver。
type stubResolver func(ctx context.Context, host string) ([]netip.Addr, error)

func (f stubResolver) LookupNetIP(ctx context.Context, _, host string) ([]netip.Addr, error) {
	return f(ctx, host)
}

// fixed は、どの名前にも同じ IP を返す Resolver。
func fixed(ips ...string) stubResolver {
	addrs := make([]netip.Addr, len(ips))
	for i, s := range ips {
		addrs[i] = netip.MustParseAddr(s)
	}
	return func(context.Context, string) ([]netip.Addr, error) { return addrs, nil }
}

// allowAll は、IP の検査を外す (テストの上流は loopback にあるため。検査そのものを試すテストでは使わない)。
func allowAll(s *Server) { s.forbid = func(netip.Addr) bool { return false } }

// syncBuf は、goroutine から書ける監査の出力先。fail が真の間、書き込みは失敗する。
type syncBuf struct {
	mu   sync.Mutex
	b    bytes.Buffer
	fail atomic.Bool
}

func (s *syncBuf) Write(p []byte) (int, error) {
	if s.fail.Load() {
		return 0, errors.New("audit: 書けない")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// records は、監査の全行を JSON として読む。JSON でない行や、未知のフィールドがあれば、失敗にする。
func (s *syncBuf) records(t *testing.T) []auditRecord {
	t.Helper()
	var out []auditRecord
	for _, line := range strings.Split(strings.TrimSuffix(s.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		var r auditRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("監査の行が JSON として読めない: %v\n行: %q", err, line)
		}
		if _, err := time.Parse(time.RFC3339Nano, r.Time); err != nil {
			t.Fatalf("監査の time が RFC 3339 でない: %q", r.Time)
		}
		out = append(out, r)
	}
	return out
}

// only は、監査がちょうど 1 行であることを確かめて、その行を返す。
func (s *syncBuf) only(t *testing.T) auditRecord {
	t.Helper()
	recs := s.records(t)
	if len(recs) != 1 {
		t.Fatalf("監査は 1 行のはず: %d 行\n%s", len(recs), s.String())
	}
	return recs[0]
}

// upstream は、127.0.0.1 で待ち受けるテスト用の上流。
type upstream struct {
	l       net.Listener
	accepts atomic.Int32
	handle  func(net.Conn)

	mu    sync.Mutex
	conns []net.Conn
	wg    sync.WaitGroup
}

func newUpstream(t *testing.T, handle func(net.Conn)) *upstream {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &upstream{l: l, handle: handle}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			u.accepts.Add(1)
			u.mu.Lock()
			u.conns = append(u.conns, c)
			u.mu.Unlock()
			u.wg.Add(1)
			go func() {
				defer u.wg.Done()
				defer c.Close()
				if u.handle != nil {
					u.handle(c)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		u.mu.Lock()
		for _, c := range u.conns {
			c.Close()
		}
		u.mu.Unlock()
		u.wg.Wait()
	})
	return u
}

func (u *upstream) port() int { return u.l.Addr().(*net.TCPAddr).Port }

// target は、Allow と CONNECT に使う "upstream.test:<port>"。名前は、Resolver が 127.0.0.1 にする。
func (u *upstream) target() string { return "upstream.test:" + strconv.Itoa(u.port()) }

// connections は、接続を受けた数。accept ループが追いつくのを、少し待つ。
func (u *upstream) connections() int {
	time.Sleep(30 * time.Millisecond)
	return int(u.accepts.Load())
}

// echo は、受けたバイトを返し、EOF で書き込み側を閉じる。
func echo(c net.Conn) {
	io.Copy(c, c)
	c.(*net.TCPConn).CloseWrite()
}

// proxy は、Unix ドメインソケットで待ち受ける、テスト中の Server。
type proxy struct {
	s     *Server
	path  string
	audit *syncBuf
	done  chan error
}

// startProxy は、cfg の Server を起動する。Audit が nil なら、p.audit に書く。tweak は、New の直後に Server を変える。
func startProxy(t *testing.T, cfg Config, tweak func(*Server)) *proxy {
	t.Helper()
	dir, err := os.MkdirTemp("", "eg") // Unix ドメインソケットの path は、短くしないと通らない
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := &proxy{path: filepath.Join(dir, "s"), audit: &syncBuf{}, done: make(chan error, 1)}
	if cfg.Audit == nil {
		cfg.Audit = p.audit
	}
	p.s = New(cfg)
	if tweak != nil {
		tweak(p.s)
	}
	l, err := net.Listen("unix", p.path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.s.Serve(l) }()
	t.Cleanup(func() {
		p.s.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("Close の後も Serve が返らない")
		}
	})
	return p
}

// forUpstream は、u だけを許可し、名前を 127.0.0.1 に解決し、IP の検査を外した Server を起動する。
// mod は Config を、tweaks は (allowAll の後に) Server を変える。
func forUpstream(t *testing.T, u *upstream, mod func(*Config), tweaks ...func(*Server)) *proxy {
	t.Helper()
	cfg := Config{Allow: []string{u.target()}, Resolver: fixed("127.0.0.1")}
	if mod != nil {
		mod(&cfg)
	}
	return startProxy(t, cfg, func(s *Server) {
		allowAll(s)
		for _, f := range tweaks {
			f(s)
		}
	})
}

// open は、proxy への接続を開く。10 秒で全体が切れる (テストが固まらないように)。
func (p *proxy) open(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("unix", p.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return c, bufio.NewReader(c)
}

// response は、応答の先頭部分。
type response struct {
	code    int
	headers string
}

// readHead は、応答の状態行とヘッダを、空行まで読む。
func readHead(br *bufio.Reader) (response, error) {
	var r response
	first := true
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return r, fmt.Errorf("応答を読めない (読めた分: %q): %w", r.headers+line, err)
		}
		line = strings.TrimSuffix(line, "\r\n")
		if first {
			f := strings.Fields(line)
			if len(f) < 2 || !strings.HasPrefix(f[0], "HTTP/1.1") {
				return r, fmt.Errorf("状態行が HTTP/1.1 でない: %q", line)
			}
			r.code, err = strconv.Atoi(f[1])
			if err != nil {
				return r, err
			}
			first = false
			continue
		}
		if line == "" {
			return r, nil
		}
		r.headers += line + "\n"
	}
}

// request は、生のリクエストを送り、応答の先頭を読む。
func (p *proxy) request(t *testing.T, raw string) (response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, br := p.open(t)
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	r, err := readHead(br)
	if err != nil {
		t.Fatal(err)
	}
	return r, c, br
}

// requestLenient は、request と同じだが、書き込みの失敗を許す。proxy が、要求を読む前に断って閉じる場合 (busy) は、
// client の write が EPIPE になりうる。応答は、閉じられた後でも読める。応答を読めなければ、code 0 を返す。
func (p *proxy) requestLenient(t *testing.T, raw string) (response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, br := p.open(t)
	io.WriteString(c, raw)
	r, _ := readHead(br)
	return r, c, br
}

// connect は、target への CONNECT が 200 になることを確かめて、トンネルを返す。
func (p *proxy) connect(t *testing.T, target string) (*net.UnixConn, *bufio.Reader) {
	t.Helper()
	r, c, br := p.request(t, connectRequest(target))
	if r.code != 200 {
		t.Fatalf("CONNECT %s = %d (200 のはず)\n監査:\n%s", target, r.code, p.audit.String())
	}
	return c.(*net.UnixConn), br
}

func connectRequest(target string) string {
	return "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
}

// eventually は、cond が真になるのを、最大 5 秒待つ。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("5 秒以内に成立しない: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

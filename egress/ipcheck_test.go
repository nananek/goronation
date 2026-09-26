package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestForbidden は、禁止する IP と、禁止しない IP を、範囲の境界で固定する。
func TestForbidden(t *testing.T) {
	forbiddenIPs := []string{
		// loopback
		"127.0.0.1", "127.255.255.254", "::1",
		// private (RFC 1918 と ULA)
		"10.0.0.0", "10.255.255.255", "172.16.0.0", "172.31.255.255", "192.168.0.0", "192.168.255.255", "fc00::1", "fd12:3456::1",
		// CGNAT (tailnet)
		"100.64.0.0", "100.100.100.100", "100.127.255.255",
		// link-local (クラウドのメタデータを含む)
		"169.254.0.1", "169.254.169.254", "fe80::1", "febf::1",
		// multicast
		"224.0.0.1", "239.255.255.255", "ff02::1", "ff05::2",
		// unspecified
		"0.0.0.0", "::",
		// IPv4-mapped IPv6
		"::ffff:127.0.0.1", "::ffff:10.1.2.3", "::ffff:172.16.0.1", "::ffff:192.168.1.1", "::ffff:100.64.0.1",
		"::ffff:169.254.169.254", "::ffff:224.0.0.1", "::ffff:0.0.0.0",
		// zone つき
		"fe80::1%eth0",
	}
	for _, s := range forbiddenIPs {
		if !forbidden(netip.MustParseAddr(s)) {
			t.Errorf("forbidden(%s) = false, want true", s)
		}
	}
	allowedIPs := []string{
		"8.8.8.8", "203.0.113.10", "160.79.104.10", "2606:4700:4700::1111", "2001:db8::1",
		// 範囲の外側の隣
		"9.255.255.255", "11.0.0.0", "172.15.255.255", "172.32.0.0", "192.167.255.255", "192.169.0.0",
		"100.63.255.255", "100.128.0.0", "169.253.255.255", "169.255.0.0", "126.255.255.255", "128.0.0.0", "223.255.255.255",
		"fbff::1", "fe00::1", "fec0::1", "feff::1",
		"::ffff:8.8.8.8", "::ffff:100.63.255.255", "::ffff:100.128.0.1",
	}
	for _, s := range allowedIPs {
		if forbidden(netip.MustParseAddr(s)) {
			t.Errorf("forbidden(%s) = true, want false", s)
		}
	}
	if !forbidden(netip.Addr{}) {
		t.Error("無効な Addr は、禁止のはず")
	}
}

// TestForbiddenLimits は、禁止しない IP (限界) を固定する。直ったら、この表と doc.go の「限界」を直す。
func TestForbiddenLimits(t *testing.T) {
	for _, s := range []string{
		"64:ff9b::a00:1", // NAT64: 10.0.0.1 を埋め込む
		"2002:7f00:1::",  // 6to4: 127.0.0.1 を埋め込む
		"0.0.0.1",        // 0.0.0.0/8 のうち、0.0.0.0 以外
		"198.18.0.1",     // ベンチマーク用
		"240.0.0.1",      // 予約
	} {
		if forbidden(netip.MustParseAddr(s)) {
			t.Errorf("forbidden(%s) = true。限界が直ったなら、doc.go の「限界」を直す", s)
		}
	}
}

// TestControlRejectsForbiddenAddress は、Control が、実際に接続する IP を検査することを、関数として固定する。
func TestControlRejectsForbiddenAddress(t *testing.T) {
	s := New(Config{Audit: io.Discard})
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "10.0.0.1:443", "100.64.0.1:443", "169.254.169.254:80", "[::ffff:127.0.0.1]:443", "[fe80::1%eth0]:443"} {
		var fe *forbiddenAddrError
		if err := s.control("tcp", addr, nil); !errors.As(err, &fe) {
			t.Errorf("control(%s) = %v, want forbiddenAddrError", addr, err)
		}
	}
	for _, addr := range []string{"203.0.113.10:443", "[2606:4700:4700::1111]:443"} {
		if err := s.control("tcp", addr, nil); err != nil {
			t.Errorf("control(%s) = %v, want nil", addr, err)
		}
	}
	// 解釈できない address は、通さない (fail-closed)。
	for _, addr := range []string{"", "localhost:443", "203.0.113.10", "203.0.113.10:x"} {
		if err := s.control("tcp", addr, nil); err == nil {
			t.Errorf("control(%q) = nil, want error", addr)
		}
	}
}

// TestDefaultDialerNeverConnectsToLoopback は、既定の dial が、loopback の実際の listener に接続しないことを固定する。
// (検査は Control が担うので、Control を外すと、この接続が成立して赤になる)
func TestDefaultDialerNeverConnectsToLoopback(t *testing.T) {
	u := newUpstream(t, echo)
	s := New(Config{Audit: io.Discard})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := s.dial(ctx, "tcp", u.l.Addr().String())
	if err == nil {
		conn.Close()
		t.Fatal("loopback に接続できてしまった")
	}
	var fe *forbiddenAddrError
	if !errors.As(err, &fe) {
		t.Errorf("err = %v, want forbiddenAddrError", err)
	}
	if n := u.connections(); n != 0 {
		t.Errorf("listener が %d 回 accept した", n)
	}
}

// TestForbiddenResolution は、許可した名前が内部の IP に解決されるとき、CONNECT を 403 で断り、接続を試みないことを確かめる。
// 名前 → IP は、resolver の差し込みで作る。dial は差し替えない (実際の Control を通る)。
func TestForbiddenResolution(t *testing.T) {
	ips := []string{
		"127.0.0.1", "10.0.0.1", "172.16.5.5", "192.168.1.1", "100.64.0.1", "100.100.100.100", "169.254.169.254", "0.0.0.0",
		"::1", "fe80::1", "fc00::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:100.64.0.1",
	}
	for _, s := range ips {
		t.Run(s, func(t *testing.T) {
			ip := netip.MustParseAddr(s)
			if ip.Is6() && !ip.Is4In6() && !ipv6Available() {
				t.Skip("この環境は IPv6 が使えない (socket の作成が Control より先に失敗する)")
			}
			u := newUpstream(t, echo)
			p := startProxy(t, Config{Allow: []string{u.target()}, Resolver: fixed(s)}, nil) // 既定の forbid と dial
			resp, _, _ := p.request(t, connectRequest(u.target()))
			if resp.code != 403 {
				t.Errorf("状態コード = %d, want 403", resp.code)
			}
			r := p.audit.only(t)
			if r.Event != "deny" || r.Reason != reasonForbiddenIP || r.IP != ip.Unmap().String() || r.Target != u.target() {
				t.Errorf("監査 = %+v, want deny/forbidden-ip/ip=%s", r, ip.Unmap())
			}
			if n := u.connections(); n != 0 {
				t.Errorf("上流へ %d 回接続した", n)
			}
		})
	}
}

func ipv6Available() bool {
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// TestControlChecksTheAddressActuallyDialed は、rebinding を模す: resolver が公開 IP を返して、事前の確認 (あれば)
// を通っても、実際に接続する先が loopback なら、Control が止める。dial の差し込みが、宛先を loopback の listener に書き換える。
func TestControlChecksTheAddressActuallyDialed(t *testing.T) {
	u := newUpstream(t, echo)
	const public = "203.0.113.10"
	p := startProxy(t, Config{Allow: []string{u.target()}, Resolver: fixed(public)}, func(s *Server) {
		strict := &net.Dialer{Control: s.control}
		s.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			ap := netip.MustParseAddrPort(address)
			if ap.Addr() != netip.MustParseAddr(public) {
				t.Errorf("dial の宛先 = %s, want resolver の答え %s", address, public)
			}
			// 「実際に接続する IP」が、解決した IP と違う。
			return strict.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ap.Port()))))
		}
	})
	resp, _, _ := p.request(t, connectRequest(u.target()))
	if resp.code != 403 {
		t.Errorf("状態コード = %d, want 403", resp.code)
	}
	if r := p.audit.only(t); r.Reason != reasonForbiddenIP || r.IP != "127.0.0.1" {
		t.Errorf("監査 = %+v", r)
	}
	if n := u.connections(); n != 0 {
		t.Errorf("loopback の上流へ %d 回接続した", n)
	}
}

// TestForbiddenCandidatesAreSkipped は、名前が [内部の IP, 公開 IP] に解決されるとき、内部には接続せず、公開 IP に接続することを固定する。
func TestForbiddenCandidatesAreSkipped(t *testing.T) {
	u := newUpstream(t, echo)
	const public = "203.0.113.10"
	var mu sync.Mutex
	var dialed []string
	p := startProxy(t, Config{Allow: []string{u.target()}, Resolver: fixed("127.0.0.1", public)}, func(s *Server) {
		s.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, address)
			mu.Unlock()
			if err := s.control(network, address, nil); err != nil {
				return nil, err
			}
			// 公開 IP への接続を、テストの上流に向ける。
			return (&net.Dialer{}).DialContext(ctx, network, u.l.Addr().String())
		}
	})
	c, br := p.connect(t, u.target())
	io.WriteString(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	if r := p.audit.only(t); r.Event != "allow" || r.IP != public {
		t.Errorf("監査 = %+v, want allow ip=%s", r, public)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(u.port())), net.JoinHostPort(public, strconv.Itoa(u.port()))}
	if !slices.Equal(dialed, want) {
		t.Errorf("dial の順序 = %v, want %v", dialed, want)
	}
}

// TestDialFailures は、許可した宛先への接続が失敗したときの応答と監査を固定する。
func TestDialFailures(t *testing.T) {
	u := newUpstream(t, echo)
	refusedPort := func() int { // 閉じた listener のポート
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}()

	tests := []struct {
		name   string
		mod    func(*Config)
		tweak  func(*Server) // nil なら、allowAll のまま
		target string
		code   int
		reason string
	}{
		{"接続を拒否される", nil, nil, "upstream.test:" + strconv.Itoa(refusedPort), 502, reasonDialFailed},
		{"名前を解決できない", func(c *Config) {
			c.Resolver = stubResolver(func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("NXDOMAIN") })
		}, nil, u.target(), 502, reasonResolveFailed},
		{"解決結果が空", func(c *Config) {
			c.Resolver = stubResolver(func(context.Context, string) ([]netip.Addr, error) { return nil, nil })
		}, nil, u.target(), 502, reasonResolveFailed},
		{"名前の解決が期限を超える", func(c *Config) {
			c.DialTimeout = 100 * time.Millisecond
			c.Resolver = stubResolver(func(ctx context.Context, _ string) ([]netip.Addr, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
		}, nil, u.target(), 504, reasonDialTimeout},
		{"接続が期限を超える", func(c *Config) { c.DialTimeout = 100 * time.Millisecond }, func(s *Server) {
			s.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("dial の ctx に期限が無い")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}, u.target(), 504, reasonDialTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tweaks []func(*Server)
			if tt.tweak != nil {
				tweaks = append(tweaks, tt.tweak)
			}
			p := forUpstream(t, u, func(c *Config) {
				c.Allow = []string{tt.target}
				if tt.mod != nil {
					tt.mod(c)
				}
			}, tweaks...)
			start := time.Now()
			resp, _, _ := p.request(t, connectRequest(tt.target))
			if resp.code != tt.code {
				t.Errorf("状態コード = %d, want %d", resp.code, tt.code)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("応答まで %v かかった", d)
			}
			if r := p.audit.only(t); r.Event != "error" || r.Reason != tt.reason || r.Target != tt.target {
				t.Errorf("監査 = %+v, want error/%s/%s", r, tt.reason, tt.target)
			}
		})
	}
}

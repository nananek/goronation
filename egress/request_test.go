package egress

import (
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSplitTarget は、宛先の解釈を、正常な形と、拒否する形で固定する。
func TestSplitTarget(t *testing.T) {
	kelvin := string(rune(0x212A)) // ケルビン記号。strings.ToLower は 'k' に落とす
	fullwidth := string(rune(0xff55))
	tests := []struct {
		in     string
		host   string
		port   uint16
		reason string
	}{
		{"api.anthropic.com:443", "api.anthropic.com", 443, ""},
		{"API.Anthropic.COM:443", "api.anthropic.com", 443, ""}, // 大文字小文字は区別しない
		{"a-b.example:65535", "a-b.example", 65535, ""},
		{"localhost:443", "localhost", 443, ""}, // 名前としては通る (loopback への解決は、dial の Control が止める)

		{"api.anthropic.com", "", 0, reasonBadTarget},       // ポートが無い
		{"api.anthropic.com:", "", 0, reasonBadTarget},      // ポートが空
		{"api.anthropic.com:0", "", 0, reasonBadTarget},     // ポート 0
		{"api.anthropic.com:00443", "", 0, reasonBadTarget}, // 先頭の 0
		{"api.anthropic.com:+443", "", 0, reasonBadTarget},
		{"api.anthropic.com:65536", "", 0, reasonBadTarget},
		{"api.anthropic.com:https", "", 0, reasonBadTarget},
		{"api.anthropic.com:443/x", "", 0, reasonBadTarget},
		{":443", "", 0, reasonBadTarget},
		{"", "", 0, reasonBadTarget},
		{"api.anthropic.com.:443", "", 0, reasonBadTarget}, // 末尾のドット (空のラベル)
		{".api.anthropic.com:443", "", 0, reasonBadTarget},
		{"api..anthropic.com:443", "", 0, reasonBadTarget},
		{"-a.example:443", "", 0, reasonBadTarget},
		{"a-.example:443", "", 0, reasonBadTarget},
		{"*.anthropic.com:443", "", 0, reasonBadTarget}, // ワイルドカードは無い
		{"api_x.example:443", "", 0, reasonBadTarget},
		{"[api.anthropic.com]:443", "", 0, reasonBadTarget}, // 名前を [] で囲んだ形
		{strings.Repeat("a", 64) + ".example:443", "", 0, reasonBadTarget},
		{strings.Repeat("a.", 130) + "com:443", "", 0, reasonBadTarget}, // 253 文字を超える
		{kelvin + ".test:443", "", 0, reasonBadTarget},                  // 非 ASCII は、小文字化で ASCII に化けさせない
		{fullwidth + "pstream.test:443", "", 0, reasonBadTarget},

		{"user@api.anthropic.com:443", "", 0, reasonUserinfo},
		{"user:pass@api.anthropic.com:443", "", 0, reasonUserinfo},
		{"@api.anthropic.com:443", "", 0, reasonUserinfo},
		{"api.anthropic.com@127.0.0.1:443", "", 0, reasonUserinfo},

		{"127.0.0.1:443", "", 0, reasonIPLiteral},
		{"8.8.8.8:443", "", 0, reasonIPLiteral}, // 公開 IP でも、IP リテラルは拒否する
		{"[::1]:443", "", 0, reasonIPLiteral},
		{"[2001:db8::1]:443", "", 0, reasonIPLiteral},
		{"[::ffff:127.0.0.1]:443", "", 0, reasonIPLiteral},
		{"[fe80::1%25eth0]:443", "", 0, reasonIPLiteral},
		{"[fe80::1%eth0]:443", "", 0, reasonIPLiteral},
		{"127.1:443", "", 0, reasonIPLiteral}, // inet_aton の省略形
		{"0x7f.0.0.1:443", "", 0, reasonIPLiteral},
		{"0x7f000001:443", "", 0, reasonIPLiteral},
		{"2130706433:443", "", 0, reasonIPLiteral},
		{"0177.0.0.1:443", "", 0, reasonIPLiteral},
		{"10.0.0.1.:443", "", 0, reasonBadTarget},
	}
	for _, tt := range tests {
		host, port, reason := splitTarget(tt.in)
		if host != tt.host || port != tt.port || reason != tt.reason {
			t.Errorf("splitTarget(%q) = (%q, %d, %q), want (%q, %d, %q)", tt.in, host, port, reason, tt.host, tt.port, tt.reason)
		}
	}
}

// TestSafeTarget は、監査に出す宛先が、安全な形だけであることを固定する。
func TestSafeTarget(t *testing.T) {
	bidi := string(rune(0x202e))
	tests := []struct{ in, want string }{
		{"api.anthropic.com:443", "api.anthropic.com:443"},
		{"[::1]:443", "[::1]:443"},
		{"user:SECRET@host:443", ""}, // userinfo (秘密) は出さない
		{"host" + bidi + ":443", ""}, // 双方向制御文字
		{"host\x00:443", ""},
		{"host name:443", ""},
		{"ホスト:443", ""},
		{strings.Repeat("a", 65), ""},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
	}
	for _, tt := range tests {
		if got := safeTarget(tt.in); got != tt.want {
			t.Errorf("safeTarget(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestPlainHTTPTarget は、平文の HTTP プロキシ要求から、宛先だけを取り出すことを固定する。
func TestPlainHTTPTarget(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://example.com/path?q=SECRET", "example.com"},
		{"http://example.com:8080/", "example.com:8080"},
		{"http://example.com", "example.com"},
		{"http://example.com?q=1", "example.com"},
		{"http://user:SECRET@example.com/", ""}, // userinfo があれば、何も出さない
		{"https://example.com/", ""},
		{"/path", ""},
		{"*", ""},
	}
	for _, tt := range tests {
		if got := plainHTTPTarget(tt.in); got != tt.want {
			t.Errorf("plainHTTPTarget(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRejections は、拒否する要求を、実際に Unix ドメインソケット越しに送って、応答と監査と「上流に届かない」ことを確かめる。
func TestRejections(t *testing.T) {
	u := newUpstream(t, echo)
	p := forUpstream(t, u, nil)
	port := u.port()
	other := "upstream.test:" + strconv.Itoa(port+1)
	pt := strconv.Itoa(port)

	tests := []struct {
		name   string
		req    string
		code   int
		reason string
		target string // 監査の target (空なら、出ないこと)
	}{
		// メソッド。
		{"GET (absolute-form)", "GET http://upstream.test:" + pt + "/ HTTP/1.1\r\nHost: upstream.test\r\n\r\n", 405, reasonMethod, "upstream.test:" + pt},
		{"GET (userinfo つき) は宛先を出さない", "GET http://user:SECRET-PW@example.com/ HTTP/1.1\r\n\r\n", 405, reasonMethod, ""},
		{"POST", "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", 405, reasonMethod, ""},
		{"OPTIONS *", "OPTIONS * HTTP/1.1\r\n\r\n", 405, reasonMethod, ""},
		{"connect (小文字)", "connect upstream.test:" + pt + " HTTP/1.1\r\n\r\n", 405, reasonMethod, ""},

		// 形式。
		{"HTTP/2 の preface", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", 400, reasonBadRequest, ""},
		{"版が無い", "CONNECT upstream.test:" + pt + "\r\n\r\n", 400, reasonBadRequest, ""},
		{"版が HTTP/1.2", "CONNECT upstream.test:" + pt + " HTTP/1.2\r\n\r\n", 400, reasonBadRequest, ""},
		{"空白が 2 つ", "CONNECT  upstream.test:" + pt + " HTTP/1.1\r\n\r\n", 400, reasonBadRequest, ""},
		{"LF だけ", "CONNECT upstream.test:" + pt + " HTTP/1.1\n\n", 400, reasonBadRequest, ""},
		{"ヘッダの行末が LF だけ", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\nHost: a\nX: b\r\n\r\n", 400, reasonBadRequest, ""},
		{"ヘッダの終わりが LF だけ", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\nHost: a\r\n\n", 400, reasonBadRequest, ""},
		{"ヘッダの折り返し", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\nHost: a\r\n b\r\n\r\n", 400, reasonBadRequest, ""},
		{"ヘッダに : が無い", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\nHost\r\n\r\n", 400, reasonBadRequest, ""},
		{"ヘッダ名が空", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\n: x\r\n\r\n", 400, reasonBadRequest, ""},
		{"ヘッダ名に空白", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\nBad Name: x\r\n\r\n", 400, reasonBadRequest, ""},
		{"ヘッダに NUL", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\nHost: a\x00b\r\n\r\n", 400, reasonBadRequest, ""},
		{"ヘッダに CR", "CONNECT upstream.test:" + pt + " HTTP/1.1\r\nHost: a\rb\r\n\r\n", 400, reasonBadRequest, ""},

		// 宛先。
		{"ポートが無い", "CONNECT upstream.test HTTP/1.1\r\n\r\n", 400, reasonBadTarget, "upstream.test"},
		{"末尾のドット", "CONNECT upstream.test.:" + pt + " HTTP/1.1\r\n\r\n", 400, reasonBadTarget, "upstream.test.:" + pt},
		{"[] で囲んだ名前", "CONNECT [upstream.test]:" + pt + " HTTP/1.1\r\n\r\n", 400, reasonBadTarget, "[upstream.test]:" + pt},
		{"ポートの先頭の 0", "CONNECT upstream.test:0" + pt + " HTTP/1.1\r\n\r\n", 400, reasonBadTarget, "upstream.test:0" + pt},
		{"userinfo", "CONNECT user:SECRET-PW@upstream.test:" + pt + " HTTP/1.1\r\n\r\n", 400, reasonUserinfo, ""},
		{"userinfo で許可名を装う", "CONNECT upstream.test:" + pt + "@evil.example:443 HTTP/1.1\r\n\r\n", 400, reasonUserinfo, ""},
		{"IPv4 リテラル (loopback)", "CONNECT 127.0.0.1:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonIPLiteral, "127.0.0.1:" + pt},
		{"IPv4 リテラル (公開)", "CONNECT 8.8.8.8:443 HTTP/1.1\r\n\r\n", 403, reasonIPLiteral, "8.8.8.8:443"},
		{"IPv6 リテラル", "CONNECT [::1]:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonIPLiteral, "[::1]:" + pt},
		{"IPv4-mapped", "CONNECT [::ffff:127.0.0.1]:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonIPLiteral, "[::ffff:127.0.0.1]:" + pt},
		{"省略形の IPv4", "CONNECT 127.1:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonIPLiteral, "127.1:" + pt},
		{"10 進の IPv4", "CONNECT 2130706433:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonIPLiteral, "2130706433:" + pt},
		{"ポート違い", "CONNECT " + other + " HTTP/1.1\r\n\r\n", 403, reasonNotAllowed, other},
		{"標準のポート", "CONNECT upstream.test:443 HTTP/1.1\r\n\r\n", 403, reasonNotAllowed, "upstream.test:443"},
		{"許可外の名前", "CONNECT evil.example:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonNotAllowed, "evil.example:" + pt},
		{"サブドメイン", "CONNECT sub.upstream.test:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonNotAllowed, "sub.upstream.test:" + pt},
		{"接尾辞が同じ名前", "CONNECT xupstream.test:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonNotAllowed, "xupstream.test:" + pt},
		{"接頭辞が同じ名前", "CONNECT upstream.test.evil.example:" + pt + " HTTP/1.1\r\n\r\n", 403, reasonNotAllowed, "upstream.test.evil.example:" + pt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(p.audit.records(t))
			resp, c, br := p.request(t, tt.req)
			if resp.code != tt.code {
				t.Errorf("状態コード = %d, want %d", resp.code, tt.code)
			}
			if !strings.Contains(resp.headers, "Connection: close") {
				t.Errorf("Connection: close が無い: %q", resp.headers)
			}
			if tt.code == 405 && !strings.Contains(resp.headers, "Allow: CONNECT") {
				t.Errorf("405 に Allow: CONNECT が無い: %q", resp.headers)
			}
			// 拒否の後は、proxy が接続を閉じる (トンネルにならない)。
			if _, err := br.ReadByte(); err == nil {
				t.Error("拒否の後も、接続が開いている")
			}
			c.Close()

			recs := p.audit.records(t)
			if len(recs) != before+1 {
				t.Fatalf("監査は 1 行増えるはず: %d → %d", before, len(recs))
			}
			r := recs[len(recs)-1]
			if r.Event != "deny" || r.Reason != tt.reason || r.Status != tt.code || r.Target != tt.target {
				t.Errorf("監査 = %+v, want event=deny reason=%s status=%d target=%q", r, tt.reason, tt.code, tt.target)
			}
		})
	}
	if n := u.connections(); n != 0 {
		t.Errorf("拒否した要求のために、上流へ %d 回接続した", n)
	}
	if s := p.audit.String(); strings.Contains(s, "SECRET") {
		t.Errorf("監査に秘密が出ている:\n%s", s)
	}
}

// TestMethodRejectedAfterHeaders は、CONNECT 以外でも、ヘッダを読み切ってから応答することを固定する
// (未読のバイトを残して閉じると、RST で応答が失われうるため)。
func TestMethodRejectedAfterHeaders(t *testing.T) {
	p := startProxy(t, Config{Allow: ClaudeHosts()}, nil)
	resp, _, _ := p.request(t, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nUser-Agent: x\r\nAccept: */*\r\n\r\n")
	if resp.code != 405 {
		t.Fatalf("状態コード = %d", resp.code)
	}
	if r := p.audit.only(t); r.Target != "example.com" {
		t.Errorf("平文の HTTP 要求の宛先が監査に出ない: %+v", r)
	}
}

// TestHeaderLimit は、ヘッダの大きさの上限を、境界で固定する。
func TestHeaderLimit(t *testing.T) {
	u := newUpstream(t, echo)
	req := connectRequest(u.target())
	pad := func(n int) string { // 全体が n バイトになるように、ヘッダを 1 つ足す
		extra := n - len(req)
		return req[:len(req)-2] + "X: " + strings.Repeat("a", extra-len("X: ")-2) + "\r\n\r\n"
	}

	t.Run("上限ちょうどは通る", func(t *testing.T) {
		p := forUpstream(t, u, func(c *Config) { c.MaxHeaderBytes = 300 })
		r := pad(300)
		if len(r) != 300 {
			t.Fatalf("テストの前提: len = %d", len(r))
		}
		if resp, _, _ := p.request(t, r); resp.code != 200 {
			t.Errorf("状態コード = %d, want 200", resp.code)
		}
	})
	t.Run("1 バイト超えると拒否", func(t *testing.T) {
		p := forUpstream(t, u, func(c *Config) { c.MaxHeaderBytes = 300 })
		resp, _, _ := p.request(t, pad(301))
		if resp.code != 431 {
			t.Errorf("状態コード = %d, want 431", resp.code)
		}
		if r := p.audit.only(t); r.Reason != reasonHeaderTooLarge || r.Event != "deny" {
			t.Errorf("監査 = %+v", r)
		}
	})
	t.Run("小さな行の合計が超える", func(t *testing.T) {
		p := forUpstream(t, u, func(c *Config) { c.MaxHeaderBytes = 300 })
		r := req[:len(req)-2] + strings.Repeat("X: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n", 5) + "\r\n"
		if resp, _, _ := p.request(t, r); resp.code != 431 {
			t.Errorf("状態コード = %d, want 431", resp.code)
		}
	})
	t.Run("改行の無い長い行", func(t *testing.T) {
		p := forUpstream(t, u, nil)
		c, br := p.open(t)
		go func() { // proxy が先に閉じるので、書き込みの失敗は無視する
			io.Copy(c, strings.NewReader("CONNECT "+strings.Repeat("a", 1<<20)))
		}()
		resp, err := readHead(br)
		if err != nil || resp.code != 431 {
			t.Errorf("応答 = %+v, err = %v, want 431", resp, err)
		}
	})
	if n := u.connections(); n != 1 {
		t.Errorf("上流への接続 = %d, want 1 (上限ちょうどの 1 回だけ)", n)
	}
}

// TestHeaderTimeout は、ヘッダが来ない (遅い) 接続を、期限で切ることを固定する。
func TestHeaderTimeout(t *testing.T) {
	u := newUpstream(t, echo)
	t.Run("何も送らない", func(t *testing.T) {
		p := forUpstream(t, u, func(c *Config) { c.HeaderTimeout = 100 * time.Millisecond })
		c, br := p.open(t)
		start := time.Now()
		resp, err := readHead(br)
		if err != nil || resp.code != 408 {
			t.Fatalf("応答 = %+v, err = %v, want 408", resp, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("期限 (100ms) から %v も後に切った", d)
		}
		c.Close()
		if r := p.audit.only(t); r.Reason != reasonHeaderTimeout {
			t.Errorf("監査 = %+v", r)
		}
	})
	t.Run("途中で止まる (slowloris)", func(t *testing.T) {
		p := forUpstream(t, u, func(c *Config) { c.HeaderTimeout = 200 * time.Millisecond })
		c, br := p.open(t)
		io.WriteString(c, "CONNECT upstream.test:1 HTTP/1.1\r\nHost: x\r\n")
		// 期限は、最初の 1 バイトからの絶対時刻。少しずつ送り続けても、延びない。
		go func() {
			for i := 0; i < 20; i++ {
				time.Sleep(50 * time.Millisecond)
				if _, err := io.WriteString(c, "X: y\r\n"); err != nil {
					return
				}
			}
		}()
		start := time.Now()
		resp, err := readHead(br)
		if err != nil || resp.code != 408 {
			t.Fatalf("応答 = %+v, err = %v, want 408", resp, err)
		}
		if d := time.Since(start); d > 900*time.Millisecond {
			t.Errorf("少しずつ送り続けると、期限 (200ms) が延びた: %v", d)
		}
	})
	if n := u.connections(); n != 0 {
		t.Errorf("上流への接続 = %d", n)
	}
}

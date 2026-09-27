package egress

import (
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestServeBothHTTPBodyTrickleEventuallyCutOff は、攻撃者視点レビュー (attack-review-40d9745) の再現テスト。
//
// 40d9745 の stallGuardBody は「読むたびに締め切りを延ばす」進捗ベースの検知で、固定の ReadTimeout を
// 付けない設計 (大きい push の転送を、進捗が続く限り妨げないため)。裏を返すと、締め切りより短い間隔で
// 1 バイトずつ送り続けるだけで、実効スループットがほぼゼロのまま、無期限に接続 (と、その分の goroutine・
// connect.sem の枠) を握り続けられる。
//
// さらに、この枠 (connect.sem) は、この PR で CONNECT と共有するようになった (Config.MaxConns は
// ServeBoth 配下では CONNECT・git/PR 合わせた上限)。つまり、git/PR 側でこのトリクルを MaxConns 本
// 張るだけで、本来 CONNECT (Web アクセス) にも回るはずの枠を、無期限に、無視できるほどの帯域で奪える
// (このテスト自体は git/PR 側の 1 本だけを見るが、TestServeBothOtherSharesMaxConns などが示す通り、
// 枠は CONNECT と共有なので、この 1 本が「刺さったまま」であることは、そのまま CONNECT の可用性低下に
// 直結する)。
//
// このテストは、「実効スループットが極端に低い接続は、十分な猶予 (締め切りの十数倍) を与えても、
// いずれ切られるべき」という期待を書く。現状の実装 (進捗さえあれば無期限に延びる) では、この期待は
// 満たされず、Fatal で落ちる。
func TestServeBothHTTPBodyTrickleEventuallyCutOff(t *testing.T) {
	const timeout = 50 * time.Millisecond
	connect := New(Config{Audit: io.Discard, HeaderTimeout: timeout, IdleTimeout: timeout})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // Content-Length に届くまで読もうとし続ける (実際には届かない)
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

	// 締め切り (50ms) の 20 倍 (1 秒) にわたって、締め切りより短い間隔 (30ms) で 1 バイトずつ送り続ける。
	// 合計で送る本文は 30 バイト強にしかならない (Content-Length: 1000000 には遠く及ばない)。
	deadline := time.Now().Add(20 * timeout)
	cutOff := false
	for time.Now().Before(deadline) {
		if _, err := c.Write([]byte{'x'}); err != nil {
			cutOff = true // 書き込みが失敗した = サーバに切られた
			break
		}
		time.Sleep(timeout * 3 / 5) // 締め切りより短い間隔で「進捗」を続ける
	}
	if !cutOff {
		// 書き込み側では切られたと分からなかった場合、読み側 (応答 or EOF) でも確認する。
		c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		buf := make([]byte, 16)
		n, err := c.Read(buf)
		switch {
		case err == io.EOF:
			cutOff = true
		case err == nil && n > 0 && strings.HasPrefix(string(buf[:n]), "HTTP/1.1 4"):
			cutOff = true
		case os.IsTimeout(err):
			cutOff = false
		}
	}
	if !cutOff {
		t.Fatalf("実効スループットがほぼゼロ (締め切り(%s)の20倍で30バイト強) の接続が、進捗さえ続けば無期限に生き続けた (トリクルによる stall guard の回避)。この枠は CONNECT と共有 (Config.MaxConns) なので、そのまま CONNECT の可用性低下に繋がる。", timeout)
	}
}

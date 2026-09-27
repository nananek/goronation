package egress

import (
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestServeBothHTTPHeaderTimeoutEnforced は、攻撃者視点レビュー (attack-review-bc6d03a) の再現テスト。
//
// ServeBoth は、CONNECT 用に Config.HeaderTimeout (ヘッダを読み終えるまでの期限) を持つが、この期限は
// dispatch の「最初の行」だけに使われ、その後 c.SetReadDeadline(time.Time{}) で消えてしまう。git・PR
// (other http.Handler) へ振り分けられた接続を Serve する httpSrv (&http.Server{Handler: other}) には、
// ReadHeaderTimeout などが一切設定されないため、最初の行 (接頭辞に一致する GET/POST) だけ送って、その後
// ヘッダの続き (終端の空行) を送らないクライアントを、HeaderTimeout を過ぎても切れない (Slowloris)。
//
// このテストは、CONNECT 経路と同じように、git/PR 経路でも HeaderTimeout が効くべきだ、という期待を書く。
// 現状の実装では、HeaderTimeout を過ぎても接続が生きたままになり、Fatal で落ちる。
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

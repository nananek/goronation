package egress

import (
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestServeBothHTTPBodyStallEnforced は、攻撃者視点レビュー (attack-review-d9fe8c5) の再現テスト。
//
// d9fe8c5 は、ヘッダを送らない (ReadHeaderTimeout) クライアントと、要求の合間で放置する (IdleTimeout)
// クライアントには対処したが、ReadTimeout は意図して付けていない (コミットメッセージ: 「大きい push の
// 本文の転送に、ヘッダ用の短い期限を課さないため」)。この結果、ヘッダは完全かつ即座に送るが、宣言した
// Content-Length の本文を 1 バイトも送らない (あるいは極端に遅く送る) クライアントに対しては、
// HeaderTimeout・IdleTimeout のどちらも掛からない: ヘッダはもう読み終わっているので HeaderTimeout の
// 出番はなく、ハンドラが (Body を読もうとして) 実行中なので IdleTimeout の「次の要求を待つ」区間にも
// 入らない。attacker は、この形の接続を張って git-receive-pack や PR 作成の POST のヘッダだけを
// 送り、本文を送らずに握り続けるだけで、ヘッダ Slowloris と同じ効果 (goroutine・fd の溜め込み) を得られる。
//
// このテストは、既存の 2 本 (TestServeBothHTTPHeaderTimeoutEnforced・TestServeBothIdleTimeoutEnforced)
// と同じ様式で、「本文の進捗が無いまま、そこそこ寛容な期限を過ぎたら、接続を切る」ことを期待する。
// 現状の実装では、この期限を過ぎても接続が生きたままになり、Fatal で落ちる。
func TestServeBothHTTPBodyStallEnforced(t *testing.T) {
	const timeout = 100 * time.Millisecond
	connect := New(Config{Audit: io.Discard, HeaderTimeout: timeout, IdleTimeout: timeout})
	l := newTestListener(t)
	bodyReadStarted := make(chan struct{}, 1)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyReadStarted <- struct{}{}
		buf := make([]byte, 100000)
		r.Body.Read(buf) // 本文が全く来ないので、通常はここでブロックする (期限が効けば、error で戻る)
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
	// HeaderTimeout・IdleTimeout (どちらも 100ms) の 20 倍待つ。どちらかがこの経路にも (間接にでも)
	// 効いていれば、この間に接続が閉じられる (EOF) はず。
	c.SetReadDeadline(time.Now().Add(20 * timeout))
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	switch {
	case err == io.EOF:
		return // 期待どおり: 本文が進まない接続を、サーバが切った
	case os.IsTimeout(err):
		t.Fatalf("本文を全く送らない POST (ヘッダは完全に送った) が、HeaderTimeout/IdleTimeout (%s) の20倍待っても切られなかった (本文フェーズの Slowloris)", timeout)
	default:
		t.Fatalf("予期しない結果: n=%d err=%v", n, err)
	}
}

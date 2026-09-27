package egress

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestServeBothHTTPChainedTinyRequestsEvadeAbsoluteDeadline は、攻撃者視点レビュー
// (attack-review-abd50ab) の再現テスト。
//
// abd50ab の maxBodyReadDuration (絶対の締め切り) は、stallGuardBody が要求ごとに新しく作る
// stallBody (r.Body の差し替え) に対して、その要求限りで決まる (deadline: time.Now().Add(absolute))。
// つまり、この絶対締め切りは「1 つの要求の本文読み取り」を上限にするだけで、「1 本の接続」を上限には
// しない。
//
// keep-alive の同じ接続で、宣言する Content-Length を小さく保ったまま (絶対締め切りの大きさとは無関係)、
// 1 バイトずつを IdleTimeout の progress-floor より短い間隔で送って「丁度、絶対締め切りの直前に」その
// 小さい本文を完成させ、応答を受けたら即座に次の要求を送る、を繰り返すと、1 本の接続 (と、その
// connect.sem の枠) を、単発の絶対締め切りの何倍にもわたって、実効スループットほぼゼロのまま
// 握り続けられる。
//
// このテストは、「1 本の接続が、実効スループットほぼゼロのまま握られ続けられる時間には、何らかの
// 上限がある」という期待を書く。現状の実装 (絶対締め切りが要求単位) では、この期待は満たされず、
// 十分な回数の「小さい要求を繋ぎ直す」だけで、単発の絶対締め切りの数倍を超えて接続が生き続け、
// Fatal で落ちる。
func TestServeBothHTTPChainedTinyRequestsEvadeAbsoluteDeadline(t *testing.T) {
	const idleTimeout = 20 * time.Millisecond
	const maxBodyBytes = 32 << 10 // 32 KiB → 絶対締め切り ≈ 32768/65536 秒 = 500ms
	wantAbsolute := time.Duration(maxBodyBytes) * time.Second / minBodyThroughput
	if wantAbsolute <= idleTimeout {
		t.Fatalf("テストの前提が崩れている: 絶対の締め切り (%s) が IdleTimeout (%s) の floor 以下になっている", wantAbsolute, idleTimeout)
	}

	connect := New(Config{Audit: io.Discard, HeaderTimeout: idleTimeout, IdleTimeout: idleTimeout, MaxBodyBytes: maxBodyBytes})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.WriteHeader(200)
	})
	go ServeBoth(l, connect, other, "/git/")

	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const cycles = 15
	const bodyLen = 30                // 小さい declared Content-Length (絶対締め切りの大きさとは無関係)
	perByte := idleTimeout / 4        // progress-floor (IdleTimeout) より、はっきり短い間隔
	start := time.Now()
	successfulCycles := 0
	for cyc := 0; cyc < cycles; cyc++ {
		req := "POST /git/x HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(bodyLen) + "\r\n\r\n"
		if _, err := c.Write([]byte(req)); err != nil {
			break
		}
		failed := false
		for i := 0; i < bodyLen; i++ {
			if _, err := c.Write([]byte{'x'}); err != nil {
				failed = true
				break
			}
			time.Sleep(perByte)
		}
		if failed {
			break
		}
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil || n == 0 {
			break
		}
		successfulCycles++
	}
	elapsed := time.Since(start)
	t.Logf("%d/%d サイクル成功。接続の合計生存時間 = %s (単発の絶対締め切り %s の %.1f 倍)",
		successfulCycles, cycles, elapsed, wantAbsolute, float64(elapsed)/float64(wantAbsolute))

	if successfulCycles >= cycles && elapsed > 2*wantAbsolute {
		t.Fatalf("実効スループットがほぼゼロの、小さい本文の要求を %d 回繋ぎ直しただけで、1 本の接続が"+
			"絶対締め切り (%s) の 2 倍以上 (実測 %s) にわたって生き続けた: 絶対締め切りは要求単位で、"+
			"接続全体の占有時間を制限しない (トリクル攻撃を、より低頻度な「要求の繋ぎ直し」に変えられるだけ)",
			cycles, wantAbsolute, elapsed)
	}
}

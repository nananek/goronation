package gateway

import (
	"net/netip"
	"strings"
	"testing"
)

func TestTooLargeResponse(t *testing.T) {
	cases := []struct {
		cl, cap int64
		want    bool
	}{
		{0, 100, false}, // chunked (大きさ不明) は、事前には断らない
		{-1, 100, false},
		{50, 100, false},
		{100, 100, false}, // ちょうどは通す
		{101, 100, true},
		{1 << 40, 100, true},
	}
	for _, c := range cases {
		if got := tooLargeResponse(c.cl, c.cap); got != c.want {
			t.Errorf("tooLargeResponse(%d, %d) = %v, 期待 %v", c.cl, c.cap, got, c.want)
		}
	}
}

func TestDialControl(t *testing.T) {
	orig := forbiddenAddr
	t.Cleanup(func() { forbiddenAddr = orig })

	if err := dialControl("tcp", "1.2.3.4:443", nil); err != nil {
		t.Fatalf("許可されるはずの IP: %v", err)
	}
	forbiddenAddr = func(netip.Addr) bool { return true }
	if err := dialControl("tcp", "1.2.3.4:443", nil); err == nil {
		t.Fatal("forbiddenAddr が true なのに、通した")
	}
	forbiddenAddr = orig
	// 解釈できない address は、forbiddenAddr を呼ぶ前に、専用の理由で断る (forbiddenAddr の判定に、たまたま救われない
	// ことを、別の error の文言で確かめる)。
	err := dialControl("tcp", "not-an-addr", nil)
	if err == nil || !strings.Contains(err.Error(), "解釈できない") {
		t.Fatalf("解釈できない address のエラーが、専用の文言でない: %v", err)
	}
	// 実際の既定 (egress.ForbiddenAddr) が、loopback を禁止することの確認 (allowLoopback を呼ばない、素の状態)。
	if err := dialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Fatal("既定で loopback を禁止しないと、egress の CONNECT の規則と食い違う")
	}
}

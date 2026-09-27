package gateway

import (
	"net/netip"
	"testing"
)

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
	if err := dialControl("tcp", "not-an-addr", nil); err == nil {
		t.Fatal("解釈できない address を通した")
	}
	// 実際の既定 (egress.ForbiddenAddr) が、loopback を禁止することの確認 (allowLoopback を呼ばない、素の状態)。
	if err := dialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Fatal("既定で loopback を禁止しないと、egress の CONNECT の規則と食い違う")
	}
}

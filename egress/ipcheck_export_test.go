package egress

import (
	"net/netip"
	"testing"
)

func TestForbiddenAddrExported(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "224.0.0.1", "100.64.0.1", "8.8.8.8", "::1", "2001:4860:4860::8888"} {
		a := netip.MustParseAddr(s)
		if ForbiddenAddr(a) != forbidden(a) {
			t.Errorf("%s: ForbiddenAddr (%v) と forbidden (%v) が食い違う", s, ForbiddenAddr(a), forbidden(a))
		}
	}
}

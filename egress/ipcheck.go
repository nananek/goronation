package egress

import (
	"fmt"
	"net/netip"
	"syscall"
)

// cgnat は、CGNAT の共有アドレス空間 (RFC 6598)。tailnet の IP は、ここに入る。
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// forbidden は、a が、接続してはいけない IP か (loopback・private・CGNAT・link-local・multicast・unspecified)。
// IPv4-mapped IPv6 は、IPv4 として調べる。無効な値は、禁止とする。
func forbidden(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	return !a.IsValid() ||
		a.IsUnspecified() ||
		a.IsLoopback() ||
		a.IsPrivate() ||
		a.IsLinkLocalUnicast() ||
		a.IsMulticast() ||
		cgnat.Contains(a)
}

// forbiddenAddrError は、禁止された IP への接続を、dial の Control が止めたことを表す。
type forbiddenAddrError struct{ addr netip.Addr }

func (e *forbiddenAddrError) Error() string {
	return fmt.Sprintf("egress: 接続先の IP %s は禁止されている", e.addr)
}

// control は、net.Dialer の Control。接続の直前に、実際に接続する IP (名前ではなく、解決後の 1 つの IP) を検査する。
// 解釈できない address は、禁止として扱う。
func (s *Server) control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("egress: 接続先 %q を解釈できない: %w", address, err)
	}
	if s.forbid(ap.Addr()) {
		return &forbiddenAddrError{addr: ap.Addr().Unmap()}
	}
	return nil
}

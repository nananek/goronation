package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/nananek/goronation/egress"
)

// 上流 (GitHub) への接続の期限。
const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	idleConnTimeout  = 30 * time.Second
)

// upstreamUserAgent は、上流に送る、固定の User-Agent (檻の User-Agent は使わない)。
const upstreamUserAgent = "goronation-egress/1"

// errNoRedirect は、上流の応答が redirect (3xx) のとき、追わずに断ったことを表す。
var errNoRedirect = errors.New("gateway: リダイレクトは追わない")

// forbiddenAddr は、egress.ForbiddenAddr の別名。テスト (同じ package の _test.go) だけが、偽の上流 (httptest。
// loopback) を許すために差し替える。本番は、常に egress.ForbiddenAddr のまま。
var forbiddenAddr = egress.ForbiddenAddr

// newUpstreamClient は、上流への *http.Client を作る: リダイレクトを追わず、接続する IP を forbiddenAddr で検査する
// (egress の CONNECT と同じ dial-check の再利用)。TLS の証明書の検証は、標準のまま (緩めない)。
func newUpstreamClient() *http.Client {
	dialer := &net.Dialer{Timeout: dialTimeout, Control: dialControl}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: handshakeTimeout,
		IdleConnTimeout:     idleConnTimeout,
		MaxIdleConnsPerHost: 4,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errNoRedirect
		},
	}
}

// dialControl は、net.Dialer.Control。接続の直前に、実際に接続する IP を検査する。
func dialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("gateway: 接続先 %q を解釈できない: %w", address, err)
	}
	if forbiddenAddr(ap.Addr()) {
		return fmt.Errorf("gateway: 接続先の IP %s は禁止されている", ap.Addr().Unmap())
	}
	return nil
}

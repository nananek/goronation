package egress

import (
	"bufio"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// 拒否の理由。監査の reason にそのまま出る。
const (
	reasonAllowlist      = "allowlist"
	reasonBadRequest     = "bad-request"
	reasonHeaderTooLarge = "header-too-large"
	reasonHeaderTimeout  = "header-timeout"
	reasonMethod         = "method"
	reasonBadTarget      = "bad-target"
	reasonUserinfo       = "userinfo"
	reasonIPLiteral      = "ip-literal"
	reasonNotAllowed     = "not-allowed"
	reasonForbiddenIP    = "forbidden-ip"
	reasonResolveFailed  = "resolve-failed"
	reasonDialFailed     = "dial-failed"
	reasonDialTimeout    = "dial-timeout"
	reasonBusy           = "busy"
)

// readRequest は、CONNECT のリクエスト (ヘッダの終わりまで) を読み、宛先を返す。
// 形式の誤り・大きすぎる・遅すぎる・CONNECT 以外・不正な宛先は、rj で返す。
// ヘッダの後ろに続くバイト (先送りされた TLS の ClientHello など) は、br に残す。
func (s *Server) readRequest(c net.Conn, br *bufio.Reader) (host string, port uint16, rj *reject) {
	c.SetReadDeadline(time.Now().Add(s.cfg.HeaderTimeout))
	defer c.SetReadDeadline(time.Time{})

	used := 0
	line, rj := s.readLine(br, &used)
	if rj != nil {
		return "", 0, rj
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 || (parts[2] != "HTTP/1.1" && parts[2] != "HTTP/1.0") {
		return "", 0, deny(400, reasonBadRequest, "")
	}
	for {
		h, rj := s.readLine(br, &used)
		if rj != nil {
			return "", 0, rj
		}
		if h == "" {
			break
		}
		if !validHeaderLine(h) {
			return "", 0, deny(400, reasonBadRequest, "")
		}
	}

	if parts[0] != "CONNECT" {
		return "", 0, deny(405, reasonMethod, plainHTTPTarget(parts[1]))
	}
	host, port, reason := splitTarget(parts[1])
	switch reason {
	case "":
		return host, port, nil
	case reasonIPLiteral:
		return "", 0, deny(403, reason, safeTarget(parts[1]))
	default:
		return "", 0, deny(400, reason, safeTarget(parts[1]))
	}
}

// readLine は、CRLF で終わる 1 行を、CRLF を除いて返す。used は、これまでに読んだヘッダのバイト数。
// 上限を超えた行・LF だけの行・制御文字を含む行は、拒否にする。
func (s *Server) readLine(br *bufio.Reader, used *int) (string, *reject) {
	b, err := br.ReadSlice('\n')
	*used += len(b)
	switch {
	case errors.Is(err, bufio.ErrBufferFull) || *used > s.cfg.MaxHeaderBytes:
		return "", deny(431, reasonHeaderTooLarge, "")
	case err != nil:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "", deny(408, reasonHeaderTimeout, "")
		}
		return "", deny(400, reasonBadRequest, "")
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return "", deny(400, reasonBadRequest, "")
	}
	b = b[:len(b)-2]
	for _, c := range b {
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return "", deny(400, reasonBadRequest, "")
		}
	}
	return string(b), nil
}

// validHeaderLine は、"name: value" の形か (行頭の空白 = 折り返しは認めない)。値は見ない。
func validHeaderLine(h string) bool {
	i := strings.IndexByte(h, ':')
	if i <= 0 {
		return false
	}
	for _, c := range []byte(h[:i]) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// splitTarget は、authority-form の宛先 "host:port" を、host (小文字) と port に分ける。
// 宛先として受けられない形は、reason (reasonBadTarget・reasonUserinfo・reasonIPLiteral) を返す。
// IP リテラルは、[IPv6] (: を含む) と、IPv4 (末尾のラベルが数字で始まるものは、名前として受けない) を拒否する。
// 後者は、inet_aton が受ける省略形 (127.1・0x7f000001・2130706433) も含む。
func splitTarget(t string) (host string, port uint16, reason string) {
	if strings.Contains(t, "@") {
		return "", 0, reasonUserinfo
	}
	h, p, err := net.SplitHostPort(t)
	if err != nil || h == "" {
		return "", 0, reasonBadTarget
	}
	if strings.ContainsAny(h, ":%") {
		return "", 0, reasonIPLiteral
	}
	if strings.HasPrefix(t, "[") {
		return "", 0, reasonBadTarget
	}
	h = asciiLower(h)
	if !validName(h) {
		return "", 0, reasonBadTarget
	}
	if last := h[strings.LastIndexByte(h, '.')+1:]; last[0] >= '0' && last[0] <= '9' {
		return "", 0, reasonIPLiteral
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != p {
		return "", 0, reasonBadTarget
	}
	return h, uint16(n), ""
}

// validName は、小文字の DNS 名か (ラベルは 1〜63 文字の [a-z0-9-] で、- で始まり終わらない。全体は 253 文字まで)。
func validName(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// asciiLower は、A-Z だけを小文字にする (strings.ToLower は、ケルビン記号などを ASCII に落とす)。
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// plainHTTPTarget は、平文の HTTP プロキシ要求 (absolute-form の "http://host[:port]/...") の宛先を返す。
// 拒否のログから、claude が要る宛先を知るため。安全な形でなければ空 (safeTarget を通す)。
func plainHTTPTarget(t string) string {
	rest, ok := strings.CutPrefix(t, "http://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return safeTarget(rest)
}

// safeTarget は、監査に出してよい形の宛先だけを返す (短く、[0-9A-Za-z.:%_[\]-] だけ)。
// 値が秘密を含みうる形 (userinfo の "user:pass@" など) と、制御文字・非 ASCII を含む形は、空にする。
func safeTarget(t string) string {
	if len(t) > 64 {
		return ""
	}
	for _, c := range []byte(t) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(".:%_[]-", c) >= 0) {
			return ""
		}
	}
	return t
}

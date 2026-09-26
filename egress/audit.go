package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// errResolve は、名前の解決に失敗したことを表す (classifyDial が reasonResolveFailed にする)。
var errResolve = errors.New("名前を解決できない")

// reject は、CONNECT を断る判断。応答の状態コードと、監査の理由を持つ。
type reject struct {
	event  string // "deny" (方針による拒否) か "error" (許可した宛先への接続の失敗)
	code   int
	reason string
	target string // 監査に出してよい宛先。無ければ空
	ip     string // 禁止された IP。無ければ空
}

func deny(code int, reason, target string) *reject {
	return &reject{event: "deny", code: code, reason: reason, target: target}
}

// classifyDial は、dialUpstream の error を、応答の状態コードと理由にする。
func classifyDial(err error) *reject {
	var fe *forbiddenAddrError
	var ne net.Error
	switch {
	case errors.As(err, &fe):
		rj := deny(403, reasonForbiddenIP, "")
		rj.ip = fe.addr.String()
		return rj
	case errors.Is(err, errResolve):
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return &reject{event: "error", code: 504, reason: reasonDialTimeout}
		}
		return &reject{event: "error", code: 502, reason: reasonResolveFailed}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return &reject{event: "error", code: 504, reason: reasonDialTimeout}
	default:
		return &reject{event: "error", code: 502, reason: reasonDialFailed}
	}
}

// refuse は、rj を監査に書き、状態コードを応答して、c を閉じる。
func (s *Server) refuse(c net.Conn, id uint64, rj *reject) {
	defer c.Close()
	s.audit(auditRecord{Event: rj.event, Conn: id, Target: rj.target, Reason: rj.reason, Status: rj.code, IP: rj.ip})
	writeStatus(c, rj.code)
}

// auditRecord は、監査の 1 行 (JSON)。宛先は、形が検証されたものだけを入れる。ヘッダの値は、一切入れない。
type auditRecord struct {
	Time   string `json:"time"`
	Event  string `json:"event"` // allow・deny・error
	Conn   uint64 `json:"conn"`
	Target string `json:"target,omitempty"`
	Reason string `json:"reason"`
	Status int    `json:"status"`
	IP     string `json:"ip,omitempty"`
}

// audit は、rec を 1 行の JSON として、Audit に書く。
func (s *Server) audit(rec auditRecord) error {
	rec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	_, err = s.cfg.Audit.Write(b)
	return err
}

var statusText = map[int]string{
	200: "Connection Established",
	400: "Bad Request",
	403: "Forbidden",
	405: "Method Not Allowed",
	408: "Request Timeout",
	431: "Request Header Fields Too Large",
	502: "Bad Gateway",
	503: "Service Unavailable",
	504: "Gateway Timeout",
}

// writeStatus は、本文の無い応答を書く。200 以外は、接続を閉じることを伝える。
func writeStatus(c net.Conn, code int) error {
	c.SetWriteDeadline(time.Now().Add(statusTimeout))
	defer c.SetWriteDeadline(time.Time{})
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\n", code, statusText[code])
	switch code {
	case 200:
	case 405:
		resp += "Allow: CONNECT\r\nConnection: close\r\nContent-Length: 0\r\n"
	default:
		resp += "Connection: close\r\nContent-Length: 0\r\n"
	}
	_, err := c.Write([]byte(resp + "\r\n"))
	return err
}

package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nananek/goronation/agent/opencode"
)

// HTTP の transport (opencode。ADR 0051): EncodeCommand が返す HTTP 要求の記述 ({"method","path","body"} の JSON 1 行) を、実行の前に、
// transport が独立にもう一度検査する (アダプタの検査を信じない二重の防御)。実行 (ネットワーク) は、この package は持たない。

// MaxHTTPRequest は、HTTP 要求の記述の 1 行の上限 (バイト)。
const MaxHTTPRequest = 1 << 20

// HTTPRequest は、検査を通った HTTP 要求。Path は、session の下の固定の形だけ (ParseHTTPRequest)。Body は、無いとき nil、あれば JSON のオブジェクト。
type HTTPRequest struct {
	Method string
	Path   string
	Body   []byte
}

// ParseHTTPRequest は、line (EncodeCommand の 1 行) を検査して HTTPRequest にする。通すのは、次の形だけ (sid・id は opencode.ValidID):
//
//	POST   /api/session/{sid}/prompt           POST /api/session/{sid}/interrupt
//	POST   /api/session/{sid}/permission/{id}/reply
//	POST   /api/session/{sid}/form/{id}/reply  DELETE /api/session/{sid}/form/{id}
//
// JSON は厳密 (未知の欄・後ろの余計な値は拒否)・body は null か欠けるか JSON のオブジェクト・DELETE と interrupt は body 無し。
func ParseHTTPRequest(line []byte) (HTTPRequest, error) {
	if len(line) > MaxHTTPRequest {
		return HTTPRequest{}, errors.New("chat: HTTP 要求が大きすぎる")
	}
	var r struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return HTTPRequest{}, fmt.Errorf("chat: HTTP 要求が読めない: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return HTTPRequest{}, errors.New("chat: HTTP 要求の後ろに余計なものがある")
	}
	if !allowedRoute(r.Method, r.Path) {
		return HTTPRequest{}, fmt.Errorf("chat: 許可していない HTTP 要求: %s %q", r.Method, shorten(r.Path, 80))
	}
	body := bytes.TrimSpace(r.Body)
	hasBody := len(body) > 0 && !bytes.Equal(body, []byte("null"))
	if hasBody && body[0] != '{' {
		return HTTPRequest{}, errors.New("chat: HTTP 要求の body が、オブジェクトでない")
	}
	needsBody := strings.HasSuffix(r.Path, "/prompt") || strings.HasSuffix(r.Path, "/reply")
	if hasBody != needsBody {
		return HTTPRequest{}, errors.New("chat: HTTP 要求の body の有無が、この要求の形と合わない")
	}
	out := HTTPRequest{Method: r.Method, Path: r.Path}
	if hasBody {
		out.Body = append([]byte(nil), body...)
	}
	return out, nil
}

func shorten(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// allowedRoute は、method・path が、上の固定の形か。path は '/' で分け、各 id は ValidID ([A-Za-z0-9_-] だけ。'.'・'%'・'?'・制御文字を含まない)。
func allowedRoute(method, path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/session/")
	if !ok {
		return false
	}
	seg := strings.Split(rest, "/")
	if !opencode.ValidID(seg[0]) {
		return false
	}
	switch {
	case len(seg) == 2 && (seg[1] == "prompt" || seg[1] == "interrupt"):
		return method == "POST"
	case len(seg) == 4 && seg[1] == "permission" && seg[3] == "reply":
		return method == "POST" && opencode.ValidID(seg[2])
	case len(seg) == 4 && seg[1] == "form" && seg[3] == "reply":
		return method == "POST" && opencode.ValidID(seg[2])
	case len(seg) == 3 && seg[1] == "form":
		return method == "DELETE" && opencode.ValidID(seg[2])
	}
	return false
}

// ValidSessionID は、POST /api/session の応答の id を、受けてよいか (path に入る ID の形)。
func ValidSessionID(id string) bool { return opencode.ValidID(id) }

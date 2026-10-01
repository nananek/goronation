package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// SSEReader は、SSE の本文 (敵対入力) から、data: の本文 (JSON の 1 行) を取り出す。LineReader の上限・NUL・不正な UTF-8 の検査を、そのまま通す。
// event:・id:・retry:・':' のコメント・空行は読み飛ばす (opencode は data: だけ。ADR 0022)。1 つのイベントが複数の data: 行に分かれていても、
// 1 行ずつ別の本文として返すので、JSON として読めない側は、変換で捨てられる。並行には呼べない。
type SSEReader struct{ lr *LineReader }

// NewSSEReader は、r を読む SSEReader を作る。
func NewSSEReader(r io.Reader) *SSEReader { return &SSEReader{lr: NewLineReader(r, 0)} }

// Next は、次の data: の本文を返す (先頭の 1 つの空白は除く)。返すスライスは、次の Next まで有効。LineReader の ErrLineTooLong・ErrBadLine
// (その行は捨てた) と、読み切りの io.EOF・読み込みの error は、そのまま返す。
func (r *SSEReader) Next() ([]byte, error) {
	for {
		line, err := r.lr.Next()
		if err != nil {
			return nil, err
		}
		body, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		return bytes.TrimPrefix(body, []byte(" ")), nil
	}
}

// ErrRootMismatch は、SSE の最初の root の session.created が、作成した session と違う (ほかの利用者の session を、会話にしない)。
var ErrRootMismatch = errors.New("chat: SSE の root の session が、作成した session と一致しない")

// ReadSSE は、sr を、読み切る (EOF・error) か、root が一致しないとき (ErrRootMismatch) まで読み、変換できる行を会話に流す。
// root (SSE の最初の parentID の無い session.created) が、want (POST /api/session が返した ID) と一致するまでは、何も会話に渡さない
// (root の前の行は捨てる)。一致したら onRoot を 1 回呼び、その行から渡す。行の上限超過・不正な行・変換できない行は、捨てて続ける。
func (s *Session) ReadSSE(sr *SSEReader, want string, onRoot func()) error {
	rooted := false
	for {
		data, err := sr.Next()
		switch err {
		case nil:
		case ErrLineTooLong, ErrBadLine:
			continue
		default:
			return err
		}
		if !rooted {
			switch isRoot, id := parentlessSessionCreated(data); {
			case !isRoot:
				continue
			case id != want:
				return ErrRootMismatch
			}
			rooted = true
			s.Conv.OnLine(data) // root の行を先に渡す: onRoot の後に会話を使う側が、アダプタの root 未確定に当たらないように
			if onRoot != nil {
				onRoot()
			}
			continue
		}
		s.Conv.OnLine(data) // 変換できない行は、何も配られずに、error が返る。捨てる
	}
}

// parentlessSessionCreated は、data が、parentID の無い session.created ならその sessionID を返す。
func parentlessSessionCreated(data []byte) (ok bool, sessionID string) {
	var f struct {
		Type string `json:"type"`
		Data struct {
			SessionID string `json:"sessionID"`
			ParentID  string `json:"parentID"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &f) != nil || f.Type != "session.created" || f.Data.ParentID != "" {
		return false, ""
	}
	return true, f.Data.SessionID
}

// IsServerConnected は、data が、SSE の最初のイベント server.connected か。
func IsServerConnected(data []byte) bool {
	var f struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(data, &f) == nil && f.Type == "server.connected"
}

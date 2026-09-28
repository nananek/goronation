package termrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
)

// maxReadLimit は、1 メッセージの上限バイト数。pty からの 1 回の読み取り分 (数 KiB) が余裕を持って収まり、
// resize の JSON (数十バイト) も当然収まる、固定の小さい上限。
const maxReadLimit = 64 << 10

// maxDim は、resize が受理する cols・rows の上限 (現実的な端末の大きさを大きく超える値を拒む)。
const maxDim = 10000

// StatusCode は、Close に渡す WebSocket の close code (RFC 6455)。
type StatusCode = websocket.StatusCode

// StatusNormalClosure・StatusGoingAway は、Close で使う値。
const (
	StatusNormalClosure = websocket.StatusNormalClosure
	StatusGoingAway     = websocket.StatusGoingAway
)

// ErrUnsupportedMessage は、想定外の種類のメッセージ (制御用のテキストフレーム以外のテキスト、resize
// 以外の JSON) を受け取ったときの error。
var ErrUnsupportedMessage = errors.New("termrelay: 想定外のメッセージ")

// Conn は、goronation serve の端末ビュー 1 接続分の WebSocket。
type Conn struct {
	ws *websocket.Conn
}

// Accept は、r を WebSocket にアップグレードする。goronation serve は UDS 専用 (goronation-web-plan の決定) で、
// この関数の呼び手は常に goronation web の reverse proxy だけになる。信頼の境界は、UDS に繋げること自体
// (ファイルシステムの権限) にあり、HTTP の Origin ヘッダとは無関係なので、ライブラリの Origin 検証は
// 意図的に無効にする (`InsecureSkipVerify`。ブラウザから直接 TCP で叩かれていた頃の名残の検証を、
// UDS 越しの接続にそのまま当てはめても意味を持たない。doc.go の「same-origin」を参照)。
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxReadLimit)
	return &Conn{ws: ws}, nil
}

// ResizeMessage は、ブラウザから届く resize の制御メッセージ (JSON のテキストフレーム) の形。
type ResizeMessage struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// ReadLoop は、ctx が終わるか、接続が閉じるまで、届いたメッセージを振り分け続ける: バイナリフレームは
// onData に生バイト列のまま渡し、resize のテキストフレームは onResize に渡す。それ以外 (壊れた JSON・
// 不正な値・resize 以外のテキスト) は、CloseNow (close handshake を待たない即時切断) で接続を切り、
// ErrUnsupportedMessage を返す (相手が行儀よく応答する保証が無い入力なので、close handshake の応答を
// 待たない)。呼び手のゴルーチン 1 つが、ブロックして使う (中身は onData/onResize に処理を移すだけで、
// 自分では待たないようにする: 呼び手側の処理が重いと、次のメッセージの読み取りが遅れる)。
func (c *Conn) ReadLoop(ctx context.Context, onData func(data []byte), onResize func(cols, rows int)) error {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		switch typ {
		case websocket.MessageBinary:
			onData(data)
		case websocket.MessageText:
			var m ResizeMessage
			if err := json.Unmarshal(data, &m); err != nil {
				c.ws.CloseNow()
				return fmt.Errorf("%w: 制御メッセージを読めない: %v", ErrUnsupportedMessage, err)
			}
			if m.Cols <= 0 || m.Rows <= 0 || m.Cols > maxDim || m.Rows > maxDim {
				c.ws.CloseNow()
				return fmt.Errorf("%w: resize の値が不正 (cols=%d rows=%d)", ErrUnsupportedMessage, m.Cols, m.Rows)
			}
			onResize(m.Cols, m.Rows)
		default:
			c.ws.CloseNow()
			return fmt.Errorf("%w: メッセージの種類 %v", ErrUnsupportedMessage, typ)
		}
	}
}

// WriteBinary は、p (pty の生バイト列) を 1 つのバイナリフレームとして送る。
func (c *Conn) WriteBinary(ctx context.Context, p []byte) error {
	return c.ws.Write(ctx, websocket.MessageBinary, p)
}

// Close は、close フレームを送る、通常の (相手からの応答を少し待つ) 終了。
func (c *Conn) Close(code StatusCode, reason string) error {
	return c.ws.Close(code, reason)
}

// CloseNow は、close のやり取りを待たず、すぐに接続を切る (すでに読み書きが error になっている場合など)。
func (c *Conn) CloseNow() error {
	return c.ws.CloseNow()
}

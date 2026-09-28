// Package termrelaytest は、cmd/goro の結合テストが、termrelay (ADR 0005 の例外) を直接 import せずに
// 済むよう、WebSocket クライアント側 (ブラウザの代わり) の薄いヘルパーを提供する。
//
// cmd/internal/webauthn の webauthntest と同じ理由・同じ形: この package だけが coder/websocket を
// 直接 import し (tools/archtest の websocket-only-dep が強制する)、呼び手は生の Conn/MessageType を
// 知らなくてよい。プロトコルの正しさ (フレーミング・マスク処理) はライブラリに委ね、この package は
// termrelay.Conn と同じ、バイナリ=生バイト列・テキスト=resize の JSON という使い分けだけを提供する。
package termrelaytest

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/coder/websocket"
)

// Conn は、テストが端末ビューの WebSocket に繋ぐときに使う、ブラウザ側相当のクライアント接続。
type Conn struct {
	ws *websocket.Conn
}

// DialOptions は、Dial の追加の設定。ゼロ値 (nil を渡す) は、素の TCP/TLS ダイヤルで、header 無し。
type DialOptions struct {
	// Header は、handshake の要求に添えるヘッダ (Cookie・Origin など)。
	Header http.Header
	// HTTPClient は、handshake に使う http.Client (nil なら既定)。goro serve (UDS 専用) に、
	// TCP を経由せず直接繋ぐテストのために、DialContext をカスタムした Client を渡せるようにする。
	HTTPClient *http.Client
}

// Dial は、url (http(s):// でよい) へ、opts (nil でもよい) に従って WebSocket 接続する。
func Dial(ctx context.Context, url string, opts *DialOptions) (*Conn, *http.Response, error) {
	var wsOpts websocket.DialOptions
	if opts != nil {
		wsOpts.HTTPHeader = opts.Header
		wsOpts.HTTPClient = opts.HTTPClient
	}
	ws, resp, err := websocket.Dial(ctx, url, &wsOpts)
	if err != nil {
		return nil, resp, err
	}
	return &Conn{ws: ws}, resp, nil
}

// WriteBinary は、端末への入力 (キー入力) として、p をバイナリフレームで送る。
func (c *Conn) WriteBinary(ctx context.Context, p []byte) error {
	return c.ws.Write(ctx, websocket.MessageBinary, p)
}

// WriteResize は、resize の制御メッセージ (JSON のテキストフレーム) を送る。
func (c *Conn) WriteResize(ctx context.Context, cols, rows int) error {
	b, err := json.Marshal(struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}{cols, rows})
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// WriteText は、resize 以外の、任意のテキストフレームを送る (異常系のテスト用)。
func (c *Conn) WriteText(ctx context.Context, s string) error {
	return c.ws.Write(ctx, websocket.MessageText, []byte(s))
}

// ReadBinary は、次のバイナリフレームを読む (テキストフレームが来たら error)。
func (c *Conn) ReadBinary(ctx context.Context) ([]byte, error) {
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, errUnexpectedType(typ)
	}
	return data, nil
}

// errUnexpectedType は、想定外のメッセージの種類を表す error。
func errUnexpectedType(typ websocket.MessageType) error {
	return &unexpectedTypeError{typ: typ}
}

type unexpectedTypeError struct{ typ websocket.MessageType }

func (e *unexpectedTypeError) Error() string {
	return "termrelaytest: 想定外のメッセージの種類: " + e.typ.String()
}

// Close は、接続を閉じる。
func (c *Conn) Close() error {
	return c.ws.CloseNow()
}

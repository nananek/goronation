// Package termrelay は、goro serve の端末ビュー用に絞った WebSocket 接続を実装する。
//
// ADR 0005 の依存ゼロの方針の例外 (github.com/coder/websocket) を、この package とその配下
// (termrelaytest) だけに閉じ込める。バイナリフレームは pty の生バイト列 (両方向。中身は解釈しない)、
// テキストフレームはブラウザから届く resize の JSON だけ、という固定の使い分けにする (それ以外の
// テキストフレームは protocol error で閉じる)。接続を受理する前の認証 (WebAuthn のセッション cookie)
// は、この package の責務ではない: 呼び手 (cmd/goro の requireSession) が、Accept を呼ぶ前に済ませる。
//
// # 使い方
//
//	conn, err := termrelay.Accept(w, r)
//	err = conn.ReadLoop(ctx, func(data []byte) { master.Write(data) },
//		func(cols, rows int) { resize(master, cols, rows) })
//
// # 規則
//
//   - binary-is-data: バイナリフレームは、pty の生バイト列として ReadLoop の onData にそのまま渡す
//     (境界・意味づけはしない。1 回の Read が 1 回の onData 呼び出しになる)。
//   - text-is-resize: テキストフレームは `{"cols":N,"rows":N}` の JSON だけを受理する。壊れた JSON・
//     不正な値 (0 以下・上限超え)・それ以外の形は、protocol error で接続を閉じて error を返す。
//   - same-origin: Origin の検証は、ライブラリの既定 (要求の Host と一致する Origin だけを許す) に
//     任せる。goro serve の Config.Origin は、起動時に Host と一致するよう検証済みなので、追加の
//     OriginPatterns は要らない (cmd/goro の Config.Validate を参照)。
package termrelay

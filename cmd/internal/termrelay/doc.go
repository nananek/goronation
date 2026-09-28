// Package termrelay は、goronation serve の端末ビュー用に絞った WebSocket 接続を実装する。
//
// ADR 0005 の依存ゼロの方針の例外 (github.com/coder/websocket) を、この package とその配下
// (termrelaytest) だけに閉じ込める。バイナリフレームは pty の生バイト列 (両方向。中身は解釈しない)、
// テキストフレームは resize の JSON だけ、という固定の使い分けにする (それ以外のテキストフレームは
// protocol error で閉じる)。認証は、この package の責務ではない: goronation serve は UDS 専用で、繋いで
// くるのは goronation web (WebAuthn のセッション cookie を検証済み) の reverse proxy だけという前提 (UDS に
// 繋げること自体が信頼の境界。goronation-web-plan の決定)。
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
//   - no-origin-check: Origin の検証はしない (`InsecureSkipVerify`)。HTTP の Origin ヘッダは、UDS
//     越しの接続の信頼性とは無関係 (Accept の doc comment を参照)。
package termrelay

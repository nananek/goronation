// Package chat は、goronation serve --chat の、エージェントの出力を UI・API に配る側の部品 (読む側) を置く。
//
// 檻の中のエージェントの標準出力 (敵対入力) を、1 行ずつ読み (LineReader)、標準形式の封筒に写して番号を振り、Raw を落とした JSON
// にし (Feed)、メモリ上のバッファと購読者に配る (Hub)。Envelope (Raw を含む) と Command、Stream (core/agent) と agent の実装を
// 触るのは、この package だけ (tools/archtest の v0-only-in-chat・agent-only-in-chat)。web は、Event の JSON のバイト列しか見ない。
// プロセスの起動・ネットワーク・HTTP は持たない。書く側 (会話の状態機械・承認) は別の PR。決定と理由は ADR 0009。
//
// # 使い方
//
//	lr := chat.NewLineReader(stdout, 0); feed := chat.NewFeed(stream, sessionID, nil); hub := chat.NewHub(chat.HubConfig{})
//	line, _ := lr.Next(); evs, _ := feed.Decode(line); hub.Publish(evs...)
//	sub, _ := hub.Subscribe(); e, err := sub.Next(ctx) // sub.Snapshot を先に送る。終わりは io.EOF と sub.Exit()
//
// # 規則
//
//   - line-limit: 1 行は DefaultMaxLine (1 MiB) まで。超える行は貯めずに捨てる (LineReader)。詳細は各型の doc。
//   - no-raw: Event は Raw を持たない。ID・TS・Session・Seq は Stream の値を問わず上書きする (Feed)。
//   - single-line: Event の JSON は改行を含まず、DefaultMaxEvent (2 MiB) まで (Feed)。
//   - seq: 0 から 1 ずつ。Stream の error では進めない。Decode・Encode は直列 (Feed)。
//   - ring: バイト上限を超えたら古い方から捨てる。Snapshot とライブの境目に、取りこぼしも重複も無い (Hub)。
//   - slow-subscriber: 購読者ごとのキューは有界。溢れたら外す。Publish は待たない (Hub)。
//
// # 限界
//
//   - Hub は耐久ストアではない (M2 で置き換える)。未決の権限要求も、リングから押し出されうる (Hub の doc)。
//   - 出力が本物のフレームかは確かめられない (起動の transport が担う。ADR 0010)。
//
// # 関連
//
// ADR 0009・ADR 0010。
package chat

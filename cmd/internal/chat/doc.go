// Package chat は、goronation serve --chat の、エージェントの出力を UI・API に配る側の部品 (読む側) を置く。
//
// 檻の中のエージェントの標準出力 (敵対入力) を、1 行ずつ読み (LineReader)、標準形式の封筒に写して番号を振り、Raw を落とした JSON
// にし (Feed)、メモリ上のバッファと購読者に配る (Hub)。Envelope (Raw を含む) と Command を触るのは、この package だけ
// (tools/archtest の v0-only-in-chat)。Stream (core/agent) と agent の実装の import も、この package に限る (agent-only-in-chat。DecodeFrame の戻り値の型は推論で決まり、spec/v0 を import しなくても Raw に届くため)。web は、Event の JSON のバイト列しか見ない。プロセスの起動・ネットワーク・HTTP は持たない。
// 書く側 (会話の状態機械・承認) は、別の PR で足す。決定と理由は ADR 0009。
//
// # 使い方
//
//	lr := chat.NewLineReader(stdout, 0); feed := chat.NewFeed(stream, sessionID, nil); hub := chat.NewHub(chat.HubConfig{})
//	line, _ := lr.Next(); evs, _ := feed.Decode(line); hub.Publish(evs...)
//	sub, _ := hub.Subscribe(); e, err := sub.Next(ctx) // sub.Snapshot を先に送る。終わりは io.EOF と sub.Exit()
//
// # 規則
//
//   - line-limit: 1 行は DefaultMaxLine (1 MiB) まで。超える行は、貯めずに改行まで読み捨て、ErrLineTooLong にする。NUL・不正な UTF-8 の行は ErrBadLine。続けて上限個を超える空行も ErrBadLine で一度戻る (空行の洪水で Next が戻らないのを防ぐ)。
//   - no-raw: Event は Raw を持たない。Feed は Public() の JSON だけを作り、ID・TS・Session・Seq は Stream の値を問わず上書きする。
//   - single-line: Event の JSON は改行を含まない (JSON のエスケープ。敵対テキストが SSE の別のイベントを偽造できない)。< > & はエスケープしない (6 倍に膨らむため。JSON は HTML に埋め込まない)。
//   - event-limit: Event の JSON は DefaultMaxEvent (2 MiB) まで。超えたら ErrEventTooLarge で捨てる (seq は進めない)。
//   - feed-serial: Feed の Decode・Encode は Mutex で直列になる。
//   - seq: seq は 0 から 1 ずつ (durable でないものにも)。Stream の error では進めない。
//   - ring: Hub は、上限 (バイト) を超えたら古い方から捨てる。直近の 1 件は残す (捨てるのは償却で定数時間)。購読者のキューが空なら 1 件は必ず受ける。Snapshot と ライブの境目に、取りこぼしも重複も無い。
//   - slow-subscriber: 購読者ごとのキューは、件数とバイトで上限がある。溢れた購読者は外す (ErrSlowSubscriber)。Publish は待たない。
//
// # 限界
//
//   - Hub は耐久ストアではない。溢れた古い分と、プロセスの終了後の分は無い (M2 で置き換える)。FirstSeq が 0 でなければ、省略がある。
//     リングは件数でなくバイトの上限なので、エージェントが大量に出力すると、未決の permission.requested もリングから押し出されうる
//     (再読み込みでの復元はバッファに残る間だけ)。押し出されても失効しない保持は、状態機械 (未決の表) が持つ (別の PR)。
//   - 出力が本物のフレームかは確かめられない (起動の transport が担う。ADR 0010)。未決の要求の保持は、行の上限 × 未決の上限 (約 64 MiB) が最悪。
//
// # 関連
//
// ADR 0009 (配信方式)・ADR 0010 (対話の権限要求)。
package chat

// Package chat は、goronation serve --chat の、エージェントの出力を UI・API に配る側 (読む側) と、指示・承認を返す側 (書く側) の部品を置く。
//
// 檻の中のエージェントの標準出力 (敵対入力) を、1 行ずつ読み (LineReader)、標準形式の封筒に写して番号を振り、Raw を落とした JSON
// にし (Feed)、メモリ上のバッファと購読者に配る (Hub)。会話の状態機械 (Conversation) が、指示 (Send)・承認 (Resolve)・終了 (Stop) を受ける。
// Envelope (Raw を含む) と Command、Stream (core/agent) と agent の実装を触るのは、この package だけ (tools/archtest の
// v0-only-in-chat・agent-only-in-chat)。web は、Event の JSON のバイト列と型付きの要求しか見ない。プロセス・ネットワーク・HTTP は持たない。
//
// # 使い方
//
//	l, _ := chat.Agent("claude"); s := chat.NewSession(chat.SessionConfig{Launch: l, ID: id, Input: stdin, OnStop: stop})
//	s.ReadOutput(stdout); s.Finish(exit)              // 出力を 1 行ずつ変換して配り (OnLine)、状態を進める。終わったら未決を失効
//	s.Conv.Send(text); s.Conv.ResolveIn(gen, id, "allow_once"); sub, _ := s.Hub.Subscribe() // sub.Snapshot を先に送り、sub.Next(ctx) で続ける
//
// # 規則
//
//   - line-limit: 1 行は DefaultMaxLine (1 MiB) まで。超える行は貯めずに捨てる (LineReader)。opencode の SSE・HTTP の指示も同じ検査を通す (SSEReader・ParseHTTPRequest。ADR 0051)。
//   - no-raw: Event は Raw を持たず、JSON は 1 行で DefaultMaxEvent (2 MiB) まで。ID・TS・Session・Seq は上書きする (Feed)。
//   - ring: バイト上限を超えたら古い方から捨てる。Snapshot とライブの境目に、取りこぼしも重複も無い (Hub)。Store があれば、書き込みは Mutex の外の goroutine で、遅れ・溢れ・押し出しは縮退 (ADR 0027・0053)。SubscribeAfter は DB・リング・ライブの順 (ADR 0024)。
//   - bind: 承認は要求 ID に束縛し、1 回だけ有効。許可する input は、要求時に保持した値だけ (Conversation。ADR 0011)。
//   - pin: 未決の permission.requested は、リングから溢れても Snapshot に残る。上限は MaxPendingRequests (Conversation・Hub)。
//   - expire: 終了・Stop・書き込みの失敗で、未決は全部失効する。タイマーによる失効・自動停止は無い (Conversation)。
//     入力への書き込みは有界のキュー (256 行・4 MiB) で、満杯は書き込みの失敗 (QueuedWriter。ADR 0012)。
//
// # 限界
//
//   - 耐久ログは、進行中の起動の分だけ・暗号化なし (0600)。縮退したら、その起動の残りは戻らない。Store が無い・縮退した Hub は、メモリ上のリングだけ (ADR 0023・0025・0027・0053)。hello の first_seq は、GC が削った範囲の上端の次で、after のすぐ後が非 durable の欠番だと、省略の印が余計に出うる (害なし。ADR 0054)。出力が本物のフレームかは確かめられない (起動の transport が担う。ADR 0010)。
//
// # 関連
//
// ADR 0009・ADR 0010・ADR 0011・ADR 0012・ADR 0013・ADR 0014。
package chat

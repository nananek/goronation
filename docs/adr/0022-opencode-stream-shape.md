# 0022. opencode の Stream は行のまま、HTTP と SSE は transport 層が写す (M2)

- 状態: 採用
- 日付: 2026-09-30 (2026-10-01 に追記: アダプタの実装は ADR 0049・0050、transport の実装は ADR 0051)
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0008・0010・0011・0012・0018・0019、`core/agent`、`cmd/internal/chat`

## 状況

`core/agent.Stream` は、エージェントの出力の 1 行を Envelope に変え (`DecodeFrame`)、Command をエージェントの入力のバイト列にする (`EncodeCommand`)。claude は標準入出力の行だった。opencode は、SSE の event を読み、HTTP の要求で書く (ADR 0018)。`agent/` は sandbox を import しない (archtest) ので、`net/http` も持たせたくない。

opencode の SSE は `event:` 行が無く、`data:` の JSON 1 行で、種類は JSON の `type` (`permission.asked` など) が持つ。

## 決定

1. **`Stream` の port は変えない (行のまま)。** SSE と HTTP への写しは、`agent/` の外の transport 層 (`cmd/internal/chat` 側) が持つ。
2. **読み**: transport が SSE の `data:` の JSON 1 行を、そのまま `DecodeFrame` の 1 行として渡す。session の作成 (`POST /api/session`) は transport が行い、session ID を最初の行として合成して渡す。
3. **書き**: `EncodeCommand` は「HTTP 要求の記述」(`{"method","path","body"}` の JSON 1 行) を返す。transport が、これを init の中継 (ADR 0019) 経由で実行する (`/prompt`・`/permission/{rid}/reply`・`/interrupt`)。
4. **`Conversation` は claude と共通のままにする。** 差は、書く側の `io.Writer` (transport が、有界のキューで順に HTTP を実行する。失敗は `ErrWriteFailed` で会話を終える) と、`OnStop` (`interrupt` を送り、opencode の標準入力を閉じ、檻を止める) に閉じる。承認の束縛は ADR 0011・0013 のまま。
5. **SSE の切断は、会話の終了として扱う (再接続しない)。** 切断・プロセスの終了のどちらでも、未決の権限要求を失効させ、檻を止める。取りこぼし・二重承認を作らない。SSE の耐久化は M2 の範囲外。
6. **アダプタは lenient な fail-safe。** 型が想定と違う入力は、Bad 判定で raw ごと保持する (agent/claude の方針)。

## 帰結

- `Stream`・`LineReader`・`Feed`・`Hub`・`Conversation` を、claude と共通のまま使える。契約テストと archtest は変わらない。
- transport 層が、HTTP の要求の実行・キュー・タイムアウトを持つ。ここが新しい攻撃面 (承認の取り違え・偽の id) で、攻撃者視点のレビューの対象。

## 代替案

- `Stream` を、SSE event と HTTP の型に広げる: port の契約テスト・archtest・claude アダプタまで波及する。
- `Conversation` を serve 用に別の型にする: 承認の束縛が 2 系統になり、食い違いの温床になる。

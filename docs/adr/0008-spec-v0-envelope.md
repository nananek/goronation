# 0008. 標準形式 v0: エージェントのフレームを、goronation の封筒に写す

- 状態: 採用
- 日付: 2026-09-29
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (S1)、`spec/v0` の doc (語彙とフレームの対応の正)、`spec/testdata/golden` (実フレーム)

## 状況

- M1 のアダプタと M2 の SSE API・承認 UI は、標準形式が前提になる。Issue #1 の v0 ドラフトは、実フレームを見ずに書いた。
- S1 で、claude (stream-json) と opencode (`run --format json`) の実フレームを、fake provider 相手に採った。どちらも ACP ではなく、形も大きく違う。

## 決定

- 封筒は `{v, id, ts, session, seq, type, durable, data, raw?}`。`id`・`ts`・`seq` は goronation が振る (フレームには通し番号も、信頼できる時刻も無い)。
- `seq` は durable でないイベントにも振る。再送 (`after=seq`) は durable だけを返すので、欠番は異常ではない。
- イベントとコマンドは別の型にする。コマンドに `seq`・`durable` は無い。
- 語彙は ACP から借りる (StopReason・ToolCallStatus・PermissionOptionKind・ToolKind)。無いものは拡張とし、`spec/v0` の doc に書く。
- エージェント固有の値 (tool の input など) は書き換えず `data` に載せる。元のフレームは `raw` に残し、UI へは出さない (path・設定・応答の本文を含みうる)。
- 採取で分かった差は、アダプタが吸収する (対応は `spec/v0` の各定数の doc):
  - ターンの終わり: claude は `result` を出す。opencode は明示の合図が無く、権限拒否のターンは `step_finish` (reason=tool-calls) のまま終わる。アダプタが合成する。
  - 粒度: opencode は完成した part だけを出す。実行中の tool の状態は出ないので、`tool.call` と `tool.update` は常に組で出す。
  - 権限: 非対話の実行は要求を出さず、拒否の結果だけを出す。拒否は `by=policy` の `permission.resolved` に合成する。
  - 失敗: claude の失敗の `result` は `subtype=success` のまま。`is_error` を見る。
  - 副作用: opencode は最初のターンで、フレームに出ないタイトル生成の要求を、プロバイダーに別に投げる。

## 帰結

- 採れた範囲の全フレームが写せた。UI とポリシーは、エージェントごとの形を知らずに済む。
- 未採取 (部分メッセージ・対話の権限・中断・再開・MCP・サブエージェント) の語彙は、予約か無い。M1 のアダプタの前に採り、結果で語彙が変わりうる。
- opencode の粒度が、`run` の性質か、fake が全文を 1 つの delta で返したためかは、区別できていない。`serve` (S4) で確かめる。
- フレームとイベントは 1 対 1 ではない (合成する)。アダプタのテストは、fixtures から封筒の列を作って比べる。

## 代替案

- ACP をそのまま使う: ACP はクライアントとエージェントの間の JSON-RPC で、イベントストア・`seq`・再送を持たない。どちらのエージェントも、そのままでは ACP を話さない。語彙だけを借りる。
- フレームをそのまま流す: UI・ポリシー・監査が全部の形を知る必要があり、エージェントを足すたびに変わる。
- 端末の画面を解釈する: 設計ルール 9 (端末の出力を解釈しない) に反する。

## spike の結果

- 再現手順: `spec/testdata/golden/capture.sh` の冒頭 (claude 2.1.284・opencode 1.18.32)。観察は `spec/v0/golden_test.go` が固定する。
- go / no-go: 条件付き go。採れた範囲で封筒は成り立つ。条件は、未採取のものを M1 のアダプタの前に採ること。
- 成立しない場合の代替: 対話の権限が写せなければ、`permission.*` だけをエージェント別の拡張にする。封筒は変えない。

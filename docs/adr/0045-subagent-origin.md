# 0045. サブエージェントの出力には、帰属 (origin) を付ける (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (M1.5 の「新しい課題」)、ADR 0011・0016・0041、`spec/v0/envelope.go`

## 状況

claude のサブエージェント (Agent tool) の発言・tool 呼び出しは、`parent_tool_use_id` つきで、親の stream に流れる。opencode の subagent は、子の session (`session.created` の `parentID`・`agent`) で、承認は子の session ID で出る (採取: `subagent`)。goronation は、どちらも読まず、サブエージェントの出力が、メインのエージェント自身のものとして表示される。「誰の発言・どの tool か」が分からないまま、承認する恐れがある。

## 決定

1. **`Envelope` に、追加の欄 `origin` を足す**: `{id, parent}` (`omitempty`。メインのエージェント自身の出力には付けない)。id は、サブエージェントの識別子 (claude: `parent_tool_use_id`・分かれば task の ID。opencode: 子の session の ID)。parent は、起動した tool 呼び出しの call_id (分かれば)。
2. **`Public()` は、origin を残す** (UI に出す。raw とは違い、機微ではない)。`UIEnvelope` も同じ欄を持つ。
3. **対象**: message.text・tool.call・tool.update・permission.requested・form.requested・usage など、サブエージェントが出すイベント全部。UI は、origin つきのイベントを、「サブエージェント」の印とインデントで見せ、権限の要求・form には、帰属を必ず出す (どの文脈の承認かを示す)。
4. **ターンの完了は、メインのエージェントの終わりだけ**: サブエージェントの終わりを、`turn.completed` にしない。バックグラウンドのサブエージェントで、親の `result` が先に来ても、遅れて来る出力を受ける (状態機械の見直しは、PR⑤b。ADR 0011 の改訂)。
5. **未採取**: claude のサブエージェントの実フレーム (`task_*` の system フレーム・複数の `result`)。採取と、claude 側の写しは、PR⑤b。opencode の写しは、PR⑤ (`subagent` の fixtures がある)。

## 帰結

- 表示の偽装 (サブエージェントの出力が、親のものに見える) を、語彙で塞げる。実際に塞ぐのは、アダプタと UI が使ってから。
- origin は追加の欄で、付けないアダプタ・読まない UI は、今までどおり動く。

## 代替案

- `data` の中に、帰属を入れる: イベントの種類ごとに欄が要り、UI が、全てに同じ処理を書く。
- サブエージェントの出力を落とす: 承認の要求まで落ちて、tool が止まる。

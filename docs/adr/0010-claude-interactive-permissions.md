# 0010. claude の対話的な権限要求を、標準形式の permission.requested・permission.resolve に写す

- 状態: 採用
- 日付: 2026-09-29
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (M1.5・設計ルール 10・11)、PR #57 (採取 spike)、ADR 0008、`spec/v0` の各定数の doc (対応の正)、`spec/testdata/golden/claude/permission-interactive-*.jsonl`

## 状況

- ADR 0008 は、対話での権限要求・応答を未採取のまま予約にした。M1.5 (簡易な承認 UI) は、要求を人間に見せ、goronation が応答を返す必要がある。
- PR #57 で、claude 2.1.284 の実フレームを採った。当初の想定 (`--permission-prompts host` が既定で効く) は誤りで、既定のままでは `can_use_tool` の control_request は出ず、非対話と同じ即時拒否になる。
- 出すには、`--permission-prompt-tool stdio` が要る。`--help` に出ない隠しフラグで、`@anthropic-ai/claude-agent-sdk` が `canUseTool` の指定時に足す引数。`--permission-prompts` は既定のままでよい。

## 決定

1. **起動**: 対話の承認をするセッションは、claude を `--permission-prompt-tool stdio` 付きで起動する (起動は cmd の役目。アダプタは起動しない)。要求と応答は、claude の標準入出力 (goronation が親として持つ pipe) を流れる。
2. **initialize**: 最初のターンより前に、host 発の `control_request` (subtype `initialize`) を 1 行送る。アダプタは、最初の `prompt` の raw の先頭に、この行を含める (呼び手が送り忘れない)。応答 (`control_response`) は account・memory の path などを含むので、`agent.frame` (data は空) にする。
3. **要求**: `can_use_tool` の `control_request` は `permission.requested` (data は `request_id`・`call_id`・`tool_name`・`kind`・`input`・`title`)。`input` は書き換えない。`permission_suggestions`・`display_name` など、語彙が要らない値は載せない。`request_id` (claude が振る)・`tool_name` が無い、`input` がオブジェクトでない要求は、応答できないので `agent.frame` にする。**同じ `request_id` の再要求は、先の要求が決着した後でも、`agent.frame`** にする (一意性の保証は、アダプタの段)。
4. **応答**: `permission.resolve` コマンド (data は `request_id`・`outcome`)。受けるのは `allow_once` と `reject_once` だけ (`*_always` は M2 以降。error)。アダプタは、**未決の要求だけ**に、`control_response` を 1 行返す。未知・応答済み・撤回済みの `request_id` は error で、何も書かない (1 回限り)。許可する `input` (`updatedInput`) は、**要求時に保持した値だけ**から作る。拒否の `message` は固定文。クライアントが data に足した値は使わない。応答と同時に `permission.resolved` (by=human・outcome・`request_id`) を合成する。
   - 2.1.284 は、`updatedInput` の無い許可も受けた (PR⓪)。`updatedInput` に保持した input を入れる形 (SDK と同じ) でも、実物で同じ流れになった (採取が fixture と一致)。
5. **撤回と決着**: 要求 (`request_id`) には、決着 (`permission.resolved`) がちょうど 1 つ付く。claude の `control_cancel_request` (未決のものだけ) と、未決のまま `result` (ターンの終わり) が来たときは、`permission.resolved` (by=agent・outcome=`cancelled`) にする (後者は、`turn.completed` の前に、要求の順に)。`cancelled` は `PermissionOptionKind` ではない拡張 (ACP の `RequestPermissionOutcome` の cancelled と同じ意味)。未決でない ID の撤回は `agent.frame`。UI は、決着の無い `permission.requested` を「未決」とみなせる。
6. **turn.started**: data に `text` (送った prompt の全文) を載せる。標準形式に「ユーザー発言」の語彙を足さず、画面に送った文を出せる。
7. **lenient**: 未知のフレーム・subtype・型の不一致は、error にせず `agent.frame` (raw を保持) に倒す。UI も未知の type で落ちない。
8. **版**: 形を採取した版は claude **2.1.284** (`agent/claude.TestedVersion`)。テストが、fixtures の `claude_code_version` と一致することを固定する。版を上げて採り直したら、定数・fixtures・この ADR を更新する。

### 設計ルール 11・10 との関係と、承認フローの限界

- ルール 11 (檻の中でリッスンしない): `--permission-prompt-tool stdio` は claude の標準入出力を使い、ソケットをリッスンしない。外部の MCP サーバーは介在しない。PR #57 の `net.Listen` は檻の外の connectproxy で、対象外。
- 権限の判定と応答は、檻の外の goronation が行う。ルール 10 (承認は要求 ID に束縛) は、3〜5 (ID 一意・1 回限り・保持した値だけを許可) で満たす。
- **ただし、承認フローの完全性は、PR④ で標準入出力を `socketpair` にし、実測で確定するまで保証しない。** 檻の中のプロセスが、標準出力の pipe に (`/proc/<pid>/fd/1` 経由で) 書ける間は、偽の `result`・`control_cancel_request` で未決の要求を全部 cancelled にでき、偽の `can_use_tool` を人間に見せられる。当初書いた「pipe は新しい信頼の委譲ではない」は誤りで、撤回する。アダプタも PR③ の状態機械も、この偽造を完全には塞げない。他の経路 (fd の継承・`pidfd_getfd`・ptrace・`SCM_RIGHTS`) は未検証。詳細と対応計画は `agent/claude` の `Stream` の doc。

## 帰結

- UI・ポリシーは、claude の control protocol を知らずに、承認を扱える。
- 隠しフラグへの依存が増える (安定性の問題)。lenient と、版を固定するテストで、壊れたことに気づけるようにする。
- opencode の対話的な権限承認・ターンの中断 (`CommandCancel`)・`*_always` は、この ADR の範囲外。

## 代替案

- 既定の host モードのまま: `can_use_tool` が出ないので不可 (PR⓪)。
- `--permission-prompt-tool` に MCP ツールを指す: 外部のサーバーが介在する。ルール 11 が優先する stdio 方式があるので採らない。
- `updatedInput` の無い素の許可: 動くが、「保持した値だけを許可する」を表せないので、SDK と同じ形にする。

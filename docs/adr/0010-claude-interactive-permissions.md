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

### 設計ルール 11 (檻の中でリッスンしない) との関係

- ルール 11 が求めるのは、「檻の中でエージェントがソケットをリッスンしない (stdio でやり取りする実行モードを優先する)」こと。`--permission-prompt-tool stdio` は、claude 自身の標準入出力を使い、ソケットをリッスンしない。外部の MCP サーバー・第三者ツールも介在しない。
- **権限の判定と応答は、檻の外の goronation が行う** (人間が承認し、goronation が `control_response` を返す。檻の中の claude は要求を出すだけ)。pipe はもともと goronation が持っていて、新しい信頼の委譲ではない。設計ルール 10 (承認は要求 ID に束縛) とは、3・4・5 が矛盾せず、要求 ID 一意・1 回限り・保持した値だけを許可する形で満たす。
- PR #57 の `net.Listen("unix", …)` は、cmd/framecapture が檻の外に立てる connectproxy (egress) で、対象外。
- **信頼の前提と限界**: 標準出力の pipe には、檻の中の他のプロセスも (`/proc/<pid>/fd/1` 経由で) 書ける。claude の出したものでない `control_cancel_request`・`result`・`can_use_tool` のフレームが差し込まれうる。アダプタの段では防げないので、この経路を「新しい信頼の委譲ではない」とは言い切らない。対応は、起動・transport の側 (`socketpair` 等) で決める (別途検討。M1.5 の起動 PR で扱う)。
- 未実測: stdio モードの claude が何もリッスンしないかは、`/proc/net` で確かめていない (檻で回す PR で確かめる)。

## 帰結

- UI・ポリシーは、claude の control protocol を知らずに、承認を扱える。承認の取り違え (別の要求への応答・二重応答・input の差し替え) の一部を、アダプタの段で塞ぐ (会話の状態機械が、さらに上限・失効などを持つ)。
- **隠しフラグへの依存**が増える。版が上がると、形が変わる・無くなる恐れがある (ルール 11 ではなく、安定性の問題)。lenient と、版を固定するテストで、壊れたことに気づけるようにする。
- アダプタは、見た `request_id` を決着後も覚え (決定 3 の再要求の拒否のため)、その数に上限 (`maxSeenRequests`=10000) を持つ。超えた要求は `agent.frame` になり、承認できない (誤って許可はしない)。未決の数の上限は、会話の状態機械の役目。未決の閉じ方は、件数に対して線形 (`result` は届いた順に整列して閉じる)。
- opencode の対話的な権限承認は、この ADR の範囲外 (未採取。PR #57 の opencode の fixtures は、権限承認を観察していないので外した)。ターンの中断 (`CommandCancel`) も予約のまま。

## 代替案

- 既定の host モードのまま: `can_use_tool` が出ないので不可 (PR⓪)。
- `--permission-prompt-tool` に MCP ツールを指す: 外部のサーバーが介在し、ソケットのリッスンが要りうる。ルール 11 が優先する stdio 方式があるので採らない。
- `updatedInput` の無い素の許可: 動くが、「保持した値だけを許可する」を表せないので、SDK と同じ形にする。

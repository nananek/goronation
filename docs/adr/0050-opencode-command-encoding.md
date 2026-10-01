# 0050. opencode の EncodeCommand は HTTP 要求の記述を返し、承認は once・reject だけ、ID は検査済みの形だけを path に入れる (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0021・0022・0040・0049、`agent/opencode/command.go`

## 状況

opencode は HTTP で書く (ADR 0022 決定 3)。`EncodeCommand` は、transport が実行する要求の記述 (`{"method","path","body"}` の JSON 1 行) を返す。ID は opencode が振るが、path に入るので、検査が要る。

## 決定

1. **要求** (採取した `requests.ndjson` と、method・path・body が一致する):
   - `prompt` → `POST /api/session/{root}/prompt` `{"text"}`。
   - `permission.resolve` → `POST /api/session/{sid}/permission/{id}/reply` `{"decision":"once"|"reject"}`。
   - `form.resolve` answered → `POST …/form/{id}/reply` `{"answer":{…}}`、cancelled → `DELETE …/form/{id}`。
   - `cancel` → `POST /api/session/{root}/interrupt`。body が無い要求は `null`。
2. **sid は、要求が出た session** (子の要求は子の session ID)。root が未確定なら prompt・cancel は error。
3. **`allow_always`・`reject_always` は error** (ADR 0021 決定 3)。未決でない ID (未知・応答済み・失効済み・form の ID) は error で、何も書かず、要求を消費しない (1 回限り)。
4. **許可できない要求**: 詳細が切れた (`details_truncated`) 要求への `allow_once` は error (拒否はできる)。見せていないものを、承認させない (ADR 0016 決定 8)。詳細に出さない metadata の欄 (既知は files・url・query・root・format) がある要求・resources が空の要求も、切れた要求と同じ扱い (未観測の action・版の更新で増える欄は、fail closed)。permission.asked の top-level も、採取した欄 (id・sessionID・action・resources・metadata・save・source{id,messageID,type}) の外があれば同じ (採取し直すまで fail closed)。
5. **form の回答は、要求時に保持した form に対して検査する** (`FormResolve.Validate`: key・型・options・必須)。通らなければ error。multiselect は配列のまま返す (採取: `question-multiple`)。
6. **path に入る ID は `[A-Za-z0-9_-]` の 1〜128 文字だけ** (`validID`)。`/`・`..`・空白・制御文字・`%` を含む ID は、要求・session・form として受けず、承認・応答に使えない。`url.PathEscape` も通す。
7. **合成するイベント**: `prompt` は `turn.started`、`permission.resolve` は `permission.resolved`、`form.resolve` は `form.resolved` (by=human。子の要求は Origin つき)。`Command.Data` の未知の欄は無視する。`content_hash` は見ない (Conversation が先に照合する。ADR 0042)。

## 帰結

- transport (PR⑥) は、この JSON を読んで実行するだけでよい。認可の判断は、アダプタと Conversation にある。
- opencode の ID の形が変わる (記号が増える) と、要求が承認できなくなる。fail closed で、気づける。

## 代替案

- `always` を通す: 以後の承認が、人間の見ない内容に及ぶ (ADR 0021)。
- ID を `PathEscape` だけにする: `..` や `%2f` の扱いが、サーバー側の実装に依る。
- form の回答を key だけ検査する: 型・options・必須が、Conversation の 1 か所だけに依る。

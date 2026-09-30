# opencode 2.x の serve (HTTP + SSE) の golden fixtures

採取した版: **opencode 2.0.20** (`npm pack @opencode/cli-linux-x64@2.0.20 --ignore-scripts` の `package/bin/opencode`。npm の `opencode-ai` は 1.x で別物)。
場面ごとに `events.ndjson` (SSE の `data:` の JSON。1 行 1 イベント。到着順)・`requests.ndjson` (ホスト → サーバーの要求と応答。`{method,path,body,status,response}`)・`meta.json` (版・場面の説明・観察のメモ)。
`simple-text/tools.json` は、opencode が provider に渡す tools の JSON Schema (tool ごとの引数の正)。正規化 (`cmd/internal/framenorm` の `Serve`): ID → `<ses:1>` などの記号 (要求 → イベントの順の番号)・時刻・slug・ポート・cursor。順序は並べ替えない。

## 採取の手順

```sh
# 非 root・bwrap・Landlock が要る (root では SKIP)。出力先は書き込める場所。CI は GORO_* が無く SKIP。
go test -c -o gt.test ./cmd/goronation
GORO_REAL_OPENCODE=/path/to/opencode GORO_CAPTURE_OPENCODE_SERVE=/path/to/out GORO_REQUIRE_BWRAP=1 \
  ./gt.test -test.run TestCaptureOpencodeServe -test.v          # 全場面で 4 分ほど (error の 2 場面が 90 秒ずつ)
# GORO_CAPTURE_SCENES=simple-text,actions で場面を絞る。GORO_CAPTURE_RAW_DIR=<dir> で、正規化の前の記録も書く (漏れの確認用。コミットしない)。
```

CI が走らせるのは、コミット済みの fixtures の検査 (`TestOpencodeServeGolden`: 場面の一覧・正規化済み・秘密とホストの path が無い・場面ごとの事象)。実物の opencode での再採取は、手元・夜間。

## 採取の経路 (本番と同じ防御)

`TestRealOpencodeInCage` と同じ道具 (`opencode_serve_cage_linux_test.go` の `startRealCage`): `cageSpec` → `goronation init --relay-control --relay-token-env --landlock-connect --relay-version-prefix` → `landlock-exec` (Landlock・seccomp・no_new_privs) → opencode (読めない複製から起動)。トークンは init だけが持ち、ホストは control (socketpair) 越しの `relayClient` だけで話し、要求は init のヘッダの許可リストと起動の証明・pidfd と待ち受けの確認を通る。

## API の探索 (2.0.20)

- `GET /openapi.json` (Basic 認証) が OpenAPI を返す (約 250 KB)。`/doc` は HTML。`/api/openapi.json`・`/api/doc` は 404。
- 使ったルート: `GET /api/info`・`POST /api/session` (`permissions`)・`POST /api/session/{sid}/prompt` (`{"text"}`)・`GET /api/event` (SSE)・`POST /api/session/{sid}/permission/{id}/reply` (`{"decision":once|always|reject}`・204)・`POST /api/session/{sid}/interrupt` (`{"interrupted":true}`)・`GET /api/permission/request`・`GET /api/session/{sid}/permission[/{id}]`・`GET /api/session/{sid}`・`GET /api/session/active`・`GET /api/session/{sid}/inbox`・`GET /api/session/{sid}/message`・`GET /api/session/{sid}/form`・`GET /api/form`・`POST /api/session/{sid}/form/{formID}/reply` (`{"answer":{…}}`・204)・`DELETE /api/session/{sid}/form/{formID}` (204)。
- **question は、form の仕組み** (`/form`)。`question.*` のイベント・`…/question/{id}/reply` は 2.0.20 に無い。question の tool は、先に permission (action `question`) を求め、承認後に `form.created` (`metadata.kind: question`) が出る。
- SSE は `data:` の行だけ (`id:`・`event:` の行は来ない。`: heartbeat` のコメント行が時々来るが、記録しない)。1 行の JSON の中に `id` (evt_…)・`type`・`created`・`data`・`durable` (aggregateID・seq)・`location`)。

## 観察したこと / 観察できなかったこと

観察: 場面の一覧と事象は、各 `meta.json` の `note` と `TestOpencodeServeGolden`。tool は `shell`・`write`・`edit`・`read`・`glob`・`grep`・`webfetch`・`websearch`・`skill`・`execute`・`subagent`・`question`。承認が出ない: `execute` (コード実行。中で呼ぶ tool の承認は未確認)・2 回目以降の always の rule。action は `edit` (write も)・`read`・`external_directory`・`glob`・`grep`・`webfetch`・`websearch`・`skill`・`subagent`・`question`・`shell`。

観察できなかった (推測で埋めない):
- **glob・grep の成功の形**: 檻に ripgrep が無く、承認の後に `ripgrep execution failed`。
- **`session.tool.input.delta`・推論 (`reasoning`)・`session.text` の細かい分割**: fake provider は tool の引数・本文を 1 チャンクで返す。実 provider の分割は見ていない。
- **`permission.asked` の `message`・`metadata` の全種類**: 観察した action のものだけ。
- **execute の中から呼ぶ tool** (code mode) の承認・進捗。
- **`session.execution.interrupted` の `reason`**: user (interrupt)・shutdown (承認の reject・form の取り消し) を観察。他の値は不明。
- opencode の再接続・`Last-Event-ID` の挙動 (SSE の再開)・認証の失敗・並行する session。
- 実物の provider・実ネットワーク・Landlock の下での実 provider の通信 (fake の HTTP の egress だけ)。

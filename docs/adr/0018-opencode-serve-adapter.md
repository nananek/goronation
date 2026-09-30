# 0018. opencode アダプタ (2.x の serve --stdio) の設計: 認証・中継・Landlock・承認・Stream の形 (M2)

- 状態: 採用
- 日付: 2026-09-30 (同日に、対象を 1.18.x から 2.x に改めて書き直した)
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (「`serve` の TCP リッスンを受け入れる決定」「`serve` の防御の設計」)、ADR 0008・0009・0010・0011・0012・0013・0017、`core/agent`、`cmd/internal/chat`、`cmd/goronation/init.go`

## 状況

opencode 対応を、claude の次に優先して進める。opencode の対話的な権限承認は、HTTP + SSE のサーバーにだけある。サーバーは TCP (loopback) で待ち受け、認証はパスワードだけなので、檻の中の他プロセス (エージェント自身の tool の子) が、パスワードを読むか、ポートへ繋ぐと、自分で承認を通せる。この設計は、それを断つ前提で組む。

**対象の世代は 2.x (最新) とする。** 最初は、この作業環境にあった 1.18.33 (npm の `opencode-ai`) で測って、1.18.x 固定にしかけたが、誤りだった。2.x は別の配布物で、`opencode-ai` の latest (1.18.33) とは別に、npm の **`@opencode/cli`** (latest 2.0.20。実体は `@opencode/cli-linux-x64`) として出ている (GitHub の Releases に 2.x は無く、タグ `v2.0.0`〜`v2.0.20` だけがある)。`opencode-ai` だけを見て「最新は 1.18.33」と判断しないこと。2.x は、CLI・サーバー・API の形が大きく違う (下の実測)。1.18.x は対象外で、比較のために測った結果だけを残す。

この ADR は、コードを書く前の設計と、その根拠になった実測を固定する (PR 分割の計画は、末尾「PR 分割」。計画の原本は共有メモ `plan-opencode` だが、あとに残すのはこの ADR)。

## 実測 (go / no-go 判定)

環境: **opencode 2.0.20** (`@opencode/cli-linux-x64@2.0.20` の実体。`opencode --version` は `opencode v2.0.20`)、kernel 6.12.107、bwrap の檻 (`--unshare-all`・偽 HOME・非 root は `--uid 1000`)、檻の中の fake OpenAI サーバー (chat.completions の SSE で tool_call を返す)。実 LLM は呼んでいない。再現手順は末尾。

### 2.x の CLI と、背景プロセス

1. **コマンド体系が変わった。** トップ階層は「背景サービス」を前提にしている。`opencode [dir]` (TUI)・`run`・`api` は、既定で**背景サービス** (`opencode serve --service`) に繋ぎ、無ければ起動する。`serve` は「v2 API と web サーバー」で、フラグは `--hostname --port --cors --service --stdio`。`--pure` は無い。
2. **`--standalone` の有無 (実測)。** `run --format json` を、`--standalone` なしで実行すると、終了後も `opencode serve --service` が**背景に残る** (`ps` で確認)。`--standalone` ありでは、何も残らない。`serve` は `--standalone` を受け付けない (未知のフラグで使い方を出す)。`serve` 自体が前景のサーバーで、SIGTERM で背景に何も残さず終わる。
3. **`serve --stdio`**: サーバーは TCP (`--port` 指定) で待ち受けたまま、起動完了に標準出力へ `{"url":"http://127.0.0.1:<port>"}` を 1 行出し、**標準入力が閉じると終了する** (stdin を 12 秒で閉じ、約 12 秒後に rc=0 で終わるのを確認)。親 (init) が死ぬと stdin が閉じるので、孤児のサーバーが残らない。
4. `--stdio` では、パスワードの環境変数を**サーバー自身が消す** (ソースの注釈: "Keep the lease credential out of the environment inherited by tools.")。パスワードは環境変数で渡す必要がある (環境変数が無いと、ランダムな値になり、誰にも分からない)。

### 認証・API・承認

5. **認証は Basic (ユーザー名 `opencode`)。** パスワードは環境変数 **`OPENCODE_PASSWORD`** (旧名 `OPENCODE_SERVER_PASSWORD` も読む)。無認証・Bearer は 401、`-u opencode:<pw>` は 200。server 側に、環境変数以外の受け取り口は無い (ソース: `process.ts`・`server-process.ts`)。
6. **API は `/api/*` (v2) だけ。** v1 の `/event`・`/permission`・`/session/{id}/prompt_async`・`/doc` は無い (200 は web UI の HTML)。使うのは次:
   - `GET /api/info` (`{"version","pid","urls","paths"}`。認証が要る)
   - `POST /api/session` (`{"model":{"providerID","id"},"location":{"directory"},"permissions":[...]}`) → `{"data":{"id":"ses_…",…}}`
   - `POST /api/session/{sid}/prompt` (`{"text"}`。200 で受理。1.18 の `prompt_async` (204) とは違う)
   - `GET /api/event` (SSE。`event:` 行は無く、`data:` の JSON 1 行: `{"id","created","type","location","data"}`)
   - `GET /api/permission/request` (保留中の一覧)
   - `POST /api/session/{sid}/permission/{rid}/reply` (**`{"decision":"once"|"always"|"reject","message"?}`**。204。スキーマ (OpenAPI) の必須キーは `decision` で、1.18 の `{"reply":…}` とは違う。`reply` を送ったときの実際の応答は、存在しない session に対して試しただけなので未測)
   - `POST /api/session/{sid}/interrupt` (中断)
7. **承認の SSE**: `permission.asked` (`data`: `id`・`sessionID`・`action`・`resources`・`metadata`・`source: {type:"tool",messageID,id}`) → `permission.replied` (`data`: `sessionID`・`requestID`・`reply`)。reply の後、tool が実行された (`session.tool.success`)。
8. **tool と承認の名前が変わった。** fake に渡された tool は `edit glob grep question read shell skill subagent webfetch websearch write execute`。**`bash` は無く `shell`** で、承認の `action` も `"shell"`。`resources` は、コマンドを**部分コマンドに分解した一覧** (`echo …`・`env`・`grep -c …` の各要素)。設定の `permission.bash` は `shell` の別名として効く。承認の対象になる `action` の全一覧と、`execute`・`subagent`・`question` tool の扱い (承認の要求の形・別の要求の種類 (`/api/session/{sid}/form`) の有無) は**未測**。
9. **`permission` の設定の既定と優先順位 (実測。shell を 1 回実行させて、承認が要求されたか・実行されたかを見た):**
   - 設定が無いとき: **承認なしで実行される** (1.18 と同じ。明示が要る)。`"*":"ask"` は shell に効く。`bash`・`shell` のキーはどちらも効く。
   - 環境変数 `OPENCODE_CONFIG_CONTENT` (inline) は、project の `opencode.json` に勝つ (project allow + inline ask → 承認が要求された。project ask + inline allow → 実行された)。
   - **セッション作成時の `permissions` (ルールセット) は、さらに勝つ** (project allow + session ask → 承認が要求された。project ask + session allow → 実行された。project allow + inline allow + session `{"action":"*","resource":"*","effect":"ask"}` → 承認が要求された)。この経路は API の呼び出しでしか使えない。
   - 上の組み合わせは、ルール 1 件で測った。複数ルールの評価の順序・agent ごとの `permission`・`always` で保存された許可 (`/api/permission/saved`) との関係は未測。

### 檻の中での漏れと、Landlock

10. **`--stdio` では、tool の子がパスワードを継承しない。** shell tool の子で `env` にパスワードは出ない (0 件)。`--stdio` でない `serve` では、出た (1 件)。ただし **opencode 自身の `/proc/<pid>/environ` は、同じ uid の別プロセスから読める** (起動時の環境が、kernel に残る)。
11. **mode 0111 の実行ファイルにすると、opencode の `environ` は他プロセスから読めなくなる** (Bun の単体バイナリは 0111 で動く。2.0.20 で確認)。
12. **Landlock の TCP connect 制限は、2.x でも効く。** ABI 6 (kernel 6.12) で、`landlock_restrict_self` (handled = `CONNECT_TCP`、許可は egress の proxy のポートと fake provider のポートだけ) を掛けた opencode の子 (shell tool) は、サーバーのポートへ繋げない (curl は 000)。掛けていない側 (起動元) は繋がる。provider への通信は、許可したポートで動く。
13. **0111 の複製 + `serve --stdio` + Landlock を同時に掛けた起動で、tool の子からは、環境変数にも (0 件)、`/proc/*/environ` にも (読める数 0)、サーバーにも (connect 000) 届かない。**
14. **Landlock は、path の UDS への connect を止めない** (kernel の性質で、版には依らない)。FS の権利 (ABI 6 までの全 16 種) を handled にして、`/` の読み・実行だけを許可した上で、`/work/t.sock` への `connect` は成功し、`/work` への書き込みは拒否された。

### 1.18.33 との比較 (対象外。参考)

1.18.33 (npm `opencode-ai`) は、`/event`・`/permission/{id}/reply` (`{"reply"}`)・`prompt_async` の v1 の API と、`OPENCODE_SERVER_PASSWORD`、tool 名 `bash` で、`--stdio` は無く、tool の子がパスワードを継承した (そのため Landlock が必須だった)。OpenAPI は `dev` (0.0.0-dev-202609292145)・`beta` のビルドと同一で、それらは 1.x 系の開発ビルドであり、2.x ではない。

**判定: go。** 2.x は、`--stdio` (パスワードの子への継承を断つ・親が死ぬと終わる) が加わり、1.18 より扱いやすい。それでも Landlock と 0111 を残す (下の決定 5)。14 のため、**檻の中の path の UDS に、トークンを付ける中継を置くと、子がその UDS に繋いで、中継にトークンを付けさせられる** (決定 3)。

## 決定

1. **対象は opencode 2.x (npm `@opencode/cli`・2.0.x) に固定する。** 1.x は対象外。起動前に `--version` を見て、`opencode v2.0.` で始まらなければ、起動を拒否する (版が変わると前提が変わる: claude の ADR 0017、1.18 → 2.x で実際に変わった)。2.1 以降は、採取 (`cmd/framecapture`) を採り直して、範囲を広げる。
2. **Landlock が使えなければ、起動を拒否する (fail closed)。** `landlock_create_ruleset` の version が 4 未満 (kernel 6.7 未満)・無効 (ENOSYS・EOPNOTSUPP)・ruleset の作成や `restrict_self` が失敗、のどれでも、opencode を起動せず、理由を言って止まる。警告つきで許す経路は作らない。
3. **ホスト ↔ 檻の中のサーバーの経路は、名前の無い socketpair と `SCM_RIGHTS` にする** (Issue の「UDS を listen する逆方向中継」から変える)。根拠は実測 14。
   - ホストが control 用の socketpair を作り、片端を init の fd として渡す (ADR 0012 の標準入出力と同じ。init は PID 1・dumpable=0・読めない複製なので、子は `pidfd_getfd`・`/proc/pid/mem` で奪えない)。
   - HTTP の 1 要求ごとに、ホストが新しい socketpair を作り、片端を control 経由の `SCM_RIGHTS` で init に送る。init は、その fd を `127.0.0.1:<ポート>` に中継し、**`Authorization` を init が付け直す** (クライアントが付けた `Authorization`・重複・大文字小文字違いは、全部捨てて上書きする)。
   - 檻の中に、path を持つ socket・待ち受けのポートは増えない。子が繋げる入口が無い。
   - 代替 (却下): path の UDS を rw の bind に置き、mode 0600 にする (同じ uid の子は connect できる。中継がトークンを付けるので、トークンを読むより容易)。
4. **サーバーの起動は `opencode serve --stdio --hostname 127.0.0.1 --port <init が選ぶ空きポート>`。** `--standalone` は、`serve` に存在しない (実測 2)。**背景サービスを残さないための規則**: goronation が、`run`・TUI・`mini`・`api` など、背景サービスに繋ぎうるコマンドを起動するときは、**必ず `--standalone` を付ける** (chat の起動引数は、`chat.Agent` が固定する。テストで、これらのコマンドを `--standalone` なしで起動する組み立てが無いことを見る)。chat は `serve --stdio` だけを使う。init は、opencode の標準入力に pipe を繋ぎ、保持する (init が死ぬと閉じて、サーバーも終わる)。標準出力の `{"url":…}` 1 行で、起動完了とポートを確かめる。
5. **トークンは、セッションごとに init が `crypto/rand` で作る使い捨て。** 32 バイトを base64url か hex にし、init のメモリと、opencode の環境変数 `OPENCODE_PASSWORD` にだけ置く。argv・bwrap の `--setenv`・ホスト側には渡さない (ホストの `/proc/<pid>/cmdline` に出る。ホストはトークンを知る必要が無い: 3 の中継が付ける)。檻 (セッション) が終われば消える。再利用・永続化しない (背景サービスの `service` 設定にも書かせない: `--standalone`・`--stdio` のとき)。
6. **子が、トークンに届かず、サーバーにも繋げない形を、3 重にする** (実測 10〜13。2.x は、1 つ目が標準で、あとの 2 つは goronation が足す防御):
   - `--stdio` による、tool の子への環境変数の継承の断ち (opencode の挙動。**版に依存するので、テストで確かめる**)。
   - opencode は、**読めない複製 (ADR 0012・mode 0111)** から起動する (`/proc/<pid>/environ` を、他プロセスから読めなくする)。
   - 起動の前に、Landlock (TCP の connect を、egress の proxy のポートだけに制限。BIND は、最初は制限しない) と `no_new_privs` を掛けてから exec する。Go は fork と exec の間でコードを動かせないので、init が、小さな補助のサブコマンド (`goronation landlock-exec --allow-connect <port> -- <exe> <args…>`。Landlock を掛けて `execve` するだけ) を経由して起動する。init 自身には掛けない (init はサーバーのポートへ繋ぐ必要がある)。
7. **承認は、セッション作成時の `permissions` で固定する。** goronation は `POST /api/session` に `"permissions":[{"action":"*","resource":"*","effect":"ask"}]` を渡す (実測 9: project の設定・inline の環境変数のどちらにも勝つ)。API は init の中継を通る要求でしか呼べないので、子は変えられない。**ただし、ルールの評価の順序・agent ごとの設定・`always` の保存された許可との関係は未測**: PR⑥ で測るまで、「すべての tool が承認される」とは言わない。あわせて `OPENCODE_CONFIG_CONTENT` でも `permission: {"*":"ask"}` を渡す (二重)。`allow_always` は出さない (`once` と `reject` だけ)。
8. **`core/agent.Stream` の port は、行のまま変えない。** SSE と HTTP への写しは、`agent/` の外の transport 層 (`cmd/internal/chat` 側) が持つ。
   - **読み**: transport が SSE の `data:` の JSON 1 行を、そのまま `DecodeFrame` の 1 行として渡す (`event:` 行は無いので、JSON の `type` が名前)。`agent/opencode` は `net/http` を import しない (archtest の規則を保つ)。session の作成 (`POST /api/session`) は transport が行い、session ID を、最初の行として合成して渡す。
   - **書き**: `EncodeCommand` は、「HTTP 要求の記述」(`{"method","path","body"}` の JSON 1 行) を返す。transport が、これを init の中継経由で実行する (`/prompt`・`/permission/{rid}/reply`・`/interrupt`)。
   - 理由: `Stream`・`LineReader`・`Feed`・`Hub` を、claude と共通のまま使える。拡張 (Stream を、SSE event と HTTP の型に広げる) は、port の契約テスト・archtest・claude アダプタまで触る。
9. **`Conversation` は、claude と共通のままにする。** 差は、書く側の `io.Writer` (transport が、有界のキューで順に HTTP を実行する。失敗は `ErrWriteFailed` で会話を終える) と、`OnStop` (`interrupt` を送り、opencode の標準入力を閉じ、檻を止める) に閉じる。承認の束縛 (request_id・1 回限り・世代。ADR 0011・0013) は、そのまま使う。
10. **SSE の切断は、会話の終了として扱う (M2 の範囲では、再接続しない)。** 切断・プロセスの終了のどちらでも、未決の権限要求を失効させ、檻を止める (取りこぼし・二重承認を作らない)。SSE の耐久化は、別の課題。
11. **範囲外**: 承認の内容ハッシュ束縛・設定ファイル経由の承認の迂回の全体調査・`allow_always`・`question`・`form`・`subagent`・`execute` の対話 (対応しない要求は、`agent.frame` として残し、ADR 0012 の L15 と同じく、手動の「終了」で止める。**それらの tool が承認なしで実行される経路が無いかは、PR⑥ で測る**)。

## 帰結

- トークンが読まれても使えない。ただし、**守りの 2 つ (Landlock・0111) は、kernel の検査に依存する** (決定 2 の fail closed。kernel 6.7 未満では opencode を動かせない)。
- Landlock は、ポート単位で、IP は指定できない。サーバーのポートに繋げない子は、egress の proxy のポートには繋げる (それだけが許可)。opencode が、他のローカルポート (例: ローカルの MCP サーバー・2.x の内部の別プロセス) へ繋ぐなら、許可に足す必要がある (PR⑥ で、goronation が渡す MCP の設定 (`opencodeMCPInject`: `type: local` の command) と、2.x の内部通信を測る)。
- init の攻撃面が増える (HTTP の要求を、1 本ずつ受けて付け直す)。解釈は最小にする: 要求行・`Authorization` の上書き・長さの上限・接続数の上限だけ。
- 同じ uid の子は、opencode を kill できる (DoS。ADR 0012 と同じで、受け入れる)。
- 新しい fd の経路 (socketpair + `SCM_RIGHTS`) が、子に継承・奪取されないことは、PR②③の実測 (ptrace_scope=0 の非 root) で確かめる。CI の runner は scope 1 なので、対照は SKIP される (ADR 0012 の運用の約束を守る)。
- opencode の版が上がると、API・tool 名・承認の設定が変わりうる (1.18 → 2.x で、全部変わった)。`--version` の検査 (決定 1) と、フレームの採取の採り直しを、版を上げる手順に入れる。
- 2.x の環境変数のうち、`OPENCODE_DISABLE_AUTOUPDATE`・`OPENCODE_DISABLE_MODELS_FETCH` は 2.x のバイナリにも残る。**`OPENCODE_DISABLE_SHARE`・`OPENCODE_DISABLE_LSP_DOWNLOAD` は、2.0.20 のバイナリの文字列に無い** (効くかは未測。2.x で、外への通信を止める変数は、別途測る。`cmd/goronation/agent.go` の `opencodeProfile` は 1.18.32 での実測を前提に書かれている: 2.x 対応の PR で、採り直す)。

## 代替案

- **path の UDS の逆方向中継**: 決定 3 のとおり、同じ uid の子が connect できる。却下。
- **トークンをファイルで渡し、読んだら消す**: サーバー側に、ファイルから読む口が無い (実測 5)。却下。
- **`--stdio` だけに頼り、Landlock・0111 を外す**: `--stdio` の環境変数の削除は、opencode の実装の挙動で、版が変わると消えうる。`/proc/<pid>/environ` は残る。却下 (決定 6 の 3 重)。
- **Landlock だけに頼る**: トークンが、`/proc/<pid>/environ` で読めたままになる (実測 10)。kernel の検査 1 つが破れると、承認を通される。却下。
- **`Stream` を、SSE event・HTTP の型に広げる**: 決定 8。契約・archtest・claude アダプタまで波及する。却下。
- **`Conversation` を、serve 用に別の型にする**: 承認の束縛が 2 系統になり、食い違いの温床になる。却下。
- **`run --format json` を使う**: 承認を、対話的に返す口が無い (1.18.33 では自動 reject と測った。2.0.20 の `run` は承認のフラグを `--auto` しか持たない (help)。承認を返す仕組みは、実行中のサーバーへの API にしか無い)。却下。`--auto` は、承認を通さずに実行するので、論外。
- **背景サービス (`serve --service`・`--standalone` なしの `run`) を使う**: サービスが終了後も残り、パスワードが HOME の `service` 設定に保存される (2.x のソース。`ServiceConfig.password`)。セッションごとの使い捨てトークン・檻の後始末に反する。却下 (決定 4)。
- **警告つきで、Landlock なしを許す**: 却下 (決定 2)。

## 未確認 (この ADR が、成立を前提にしているもの)

- Go の init (補助の `landlock-exec`) からの Landlock の適用 (実測は python の `ctypes` で代用した)。
- host の kernel の Landlock 対応 (この環境は ABI 6)。
- 決定 7 の「`permissions` ルール 1 件で、すべての tool を `ask` にできる」こと (承認の `action` の全一覧・`execute`・`subagent`・`write`・`edit`・`webfetch` の扱い・ルールの評価の順序)。
- `question`・`form` (`/api/session/{sid}/form`) の要求の形と、実物の挙動。
- 2.x の SSE の再接続・`Last-Event-ID`・`always` と `reject` の後の挙動 (`message` の使い方)。
- 2.x の、外への通信 (更新確認・モデル一覧・共有・LSP) を止める環境変数と、`OPENCODE_CONFIG_CONTENT`・`OPENCODE_DISABLE_PROJECT_CONFIG`/`OPENCODE_CONFIG_PROJECT_DISABLE` の効き方。
- `@opencode/cli` の `dev`・`beta` (`0.0.0-dev-20318` など)、2.0.21 以降。
- socketpair + `SCM_RIGHTS` の経路を、子から奪えないこと (PR②③で実測)。

## PR 分割

| PR | 内容 | 攻撃者視点のレビュー |
|---|---|---|
| ① | この ADR | 不要 (ただし、脅威モデルは、②以降のレビューの入力) |
| ② | 中継: control socketpair・`SCM_RIGHTS`・`Authorization` の付け直し・HTTP の上限・接続数の上限 (`cmd/goronation/init.go`・`relay.go`) | 必須 (ヘッダの上書き・smuggling・fd の奪取・DoS) |
| ③ | トークン (init が生成)・`serve --stdio` の起動 (stdin の pipe・ready の確認)・`landlock-exec`・fail closed・読めない複製・版の検査 | 必須 (「読めても繋げない」を、実際に攻撃して確かめる。赤 → 緑と、再現しなかった攻撃) |
| ④ | `cmd/framecapture` で、2.x の HTTP + SSE の golden fixtures を採取 (simple-text・tool-call・permission.asked → allow/deny → replied・複数ターン・エラー・中断) | 不要 (採取の経路が、本番と同じ防御を通ること) |
| ⑤ | `agent/opencode` (アダプタ): 決定 8 の写し。lenient な fail-safe (型が想定と違えば Bad 判定で raw ごと保持) | 必須 (敵対入力の変換・permission の id の偽装・応答の取り違え) |
| ⑥ | `chat.Agent` に opencode を足す・transport・決定 7 の実測 (承認の `action` の全一覧・ルールの評価・`execute` ほか) | 必須 (承認の束縛・切断・二重承認・終了時の未決の失効) |
| ⑦ | E2E (fake provider で 1 ターン + 承認) と実物の確認。`opencodeProfile` (goronation run 用) の 2.x 対応の要否 | 総合の 1 周 |

依存: ① → ②・③ (並行可) → ④ → ⑤ → ⑥ → ⑦。

## 再現手順

前提: `bwrap`・`python3`・`curl`・npm の registry への到達。作業は使い捨ての場所で行う。ホストで opencode を実行しない (実行は、檻の中だけ)。

1. `npm pack @opencode/cli-linux-x64@2.0.20 --ignore-scripts` → `tar xzf` → `package/bin/opencode` (約 200 MB)。版の一覧: `npm view @opencode/cli dist-tags`。
2. 檻: `bwrap --unshare-all --die-with-parent --uid 1000 --gid 1000 --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/lib /lib --symlink usr/lib64 /lib64 --proc /proc --dev /dev --tmpfs /tmp --ro-bind <opencode> /opt/oc --bind <偽 HOME> /home/x --bind <work> /work --chdir /work --clearenv --setenv HOME /home/x --setenv PATH /usr/bin:/bin /bin/bash <script>` (偽 HOME・work は、uid 1000 が書ける mode にする)。
3. 檻の中の fake: `127.0.0.1:9999` の chat.completions (SSE)。tools が無いリクエストは「タイトル生成」なので、短い文を返す。tools があり、`role: tool` のメッセージがまだ無ければ、`shell` の tool_call (`{"command": …}`。`additionalProperties` は false なので、他のキーを付けない) を返す。あれば、通常の文。
4. `opencode.json` (work): `provider.fake` (`npm: @ai-sdk/openai-compatible`・`baseURL: http://127.0.0.1:9999/v1`・`apiKey: x`・`models.fake-model`)。
5. 起動: `OPENCODE_PASSWORD=<pw> ( sleep 40 | opencode serve --stdio --port 4096 )` (標準入力が閉じると終わる)。標準出力に `{"url":…}`。`GET /api/event` を `-u opencode:<pw>` で開く → `POST /api/session` (`{"model":{"providerID":"fake","id":"fake-model"},"location":{"directory":"/work"},"permissions":[{"action":"*","resource":"*","effect":"ask"}]}`) → `POST /api/session/{id}/prompt` (`{"text":"go"}`) → `GET /api/permission/request` → `POST /api/session/{id}/permission/{rid}/reply` (`{"decision":"once"}`、204)。期待: `permission.asked`・`permission.replied`・`session.tool.success` が SSE に流れる。
6. `--standalone`: `opencode run --format json -m fake/fake-model "go" </dev/null` を、ありなしで実行し、終了後に `ps` で `opencode serve --service` が残るかを見る。
7. 子からの到達: shell tool のコマンドを、`env | grep -c 'OPENCODE_.*PASSWORD'`・`curl http://127.0.0.1:4096/api/info`・`/proc/*/environ` の走査にする。`serve` を、`--stdio` あり/なし・Landlock あり/なし・実行ファイル 0111 あり/なしで比べる。
8. Landlock: `landlock_create_ruleset(NULL, 0, 1)` で ABI を取得 → `handled_access_net = CONNECT_TCP (2)` の ruleset → `LANDLOCK_RULE_NET_PORT` で許可するポートを足す → `prctl(PR_SET_NO_NEW_PRIVS)` → `landlock_restrict_self` → `execv` (syscall 番号 x86_64: 444・445・446)。
9. UDS: 同じ手順で、FS の全権 (0xffff) を handled・`/` の読み・実行だけを許可 → `/work/t.sock` (別に listen 済み) へ `connect`。
10. 設定の優先順位: `permission` を、project の `opencode.json`・`OPENCODE_CONFIG_CONTENT`・`POST /api/session` の `permissions` の組み合わせで変え、shell を 1 回実行させて、承認が要求されたか・実行されたかを見る。

fixtures は、この ADR では採らない (PR④)。

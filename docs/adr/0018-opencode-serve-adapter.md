# 0018. opencode アダプタ (serve 方式) の設計: 認証・中継・Landlock・Stream の形 (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (「`serve` の TCP リッスンを受け入れる決定」「`serve` の防御の設計」)、ADR 0008・0009・0010・0011・0012・0013・0017、`core/agent`、`cmd/internal/chat`、`cmd/goronation/init.go`

## 状況

opencode 対応を、claude の次に優先して進める。opencode の対話的な権限承認は、`run` には無く、`opencode serve` の HTTP + SSE にだけある (下の実測)。`serve` は TCP (loopback) で待ち受け、認証は環境変数のパスワードだけなので、檻の中の他プロセス (エージェント自身の tool の子) が、トークンを読むか、ポートへ直接繋ぐと、自分で承認を通せる。この設計は、それを断つ前提で組む。

この ADR は、コードを書く前の設計と、その根拠になった実測を固定する (PR 分割の計画は、末尾「PR 分割」に写した。計画の原本は共有メモ `plan-opencode` だが、あとに残すのはこの ADR)。

## 実測 (go / no-go 判定)

環境: opencode 1.18.33 (`npm pack opencode-linux-x64@1.18.33 --ignore-scripts` の実体)、kernel 6.12.107、bwrap の檻 (`--unshare-all`・偽 HOME・非 root は `--uid 1000`)、檻の中の fake OpenAI サーバー (chat.completions の SSE で bash の tool_call を返す)。実 LLM は呼んでいない。再現手順は末尾。

1. **権限承認は `serve` の HTTP + SSE にある。** `opencode.json` の `permission.bash = "ask"` のとき、`GET /event` (SSE) に `permission.asked` (`properties`: `id` (`per_…`)・`sessionID`・`permission`・`patterns`・`metadata`・`always`・`tool.{messageID,callID}`) が流れる。`GET /permission` で保留中の一覧が取れ、`POST /permission/{id}/reply` に `{"reply":"once"|"always"|"reject","message"?}` を送ると 200 で、`permission.replied` が流れて tool が実行される。旧 API `POST /session/{sid}/permissions/{pid}` (`{response}`) と、v2 (`/api/session/{sid}/permission/{rid}/reply`・`permission.v2.*`) も OpenAPI にあるが、採用は v1 の `permission.asked` / `/permission/{id}/reply` (実測したのはこちらだけ)。
2. **`run` には対話的な承認が無い。** `run --format json` は `ask` を自動で reject する (stderr に `permission requested: bash (echo hi); auto-rejecting`・JSON に `status:"error"`)。`--auto` は実行する。stdio の制御プロトコルは見つからなかった (`acp` は help を見ただけで未実測)。`run` は、stdin が開いたままの pipe だと無言でハングする (`</dev/null` が要る)。
3. **認証は Basic。** `OPENCODE_SERVER_PASSWORD` (ユーザー名は `OPENCODE_SERVER_USERNAME`、既定 `opencode`) を設定すると、無認証・Bearer・違うパスワードは 401、`-u opencode:<pw>` は 200 (再確認済み)。server 側に、環境変数以外の受け取り口は無い (バイナリに埋まった JS ソースで、`--password` 相当が `run --attach`・`attach` のクライアント側にしか無いこと、設定スキーマに password が無いことを確かめた)。
4. **設定が無いときの既定は、bash が承認なしで実行される** (`permission` を書かない `opencode.json` で、`permission.asked` は出なかった)。承認を要求させるには、`permission` を明示する必要がある。claude 2.1.285 の既定の権限モードの変更 (ADR 0017) と同種の落とし穴。
5. **トークンは、環境変数のままでは檻の中に漏れる。** 同じ uid の別プロセスから `/proc/<opencode の pid>/environ` が読める。さらに、opencode は tool の子プロセスの環境変数を消さない: bash tool の子で `env` がトークンを出す。読んだ子は、`/permission/{id}/reply` で自分の承認を通せる。
6. **mode 0111 の実行ファイルにすると、opencode の `environ` は他プロセスから読めなくなる** (dumpable=0。Bun の単体バイナリは、0111 でも起動する)。ただし子の継承した環境変数は防げない (5)。これだけでは足りない。
7. **Landlock の TCP connect 制限が効く。** ABI 6 (kernel 6.12) で、`landlock_restrict_self` (handled = `CONNECT_TCP`、許可は egress の proxy のポートと fake provider のポート) を掛けた opencode の子 (bash tool) は、`serve` のポートへ、無認証でも、盗んだトークンでも繋げない (curl は 000)。掛けていない側 (起動元) は繋がる (401・200)。provider への通信は、許可したポート経由で動く。**環境変数は読めたままでも、使えない。**
8. **Landlock は、path の UDS への connect を止めない。** FS の権利 (ABI 6 までの全 16 種) を handled にして、`/` の読み・実行だけを許可した上で、`/work/t.sock` への `connect` は成功し、`/work` への書き込みは拒否された (Landlock は効いているのに、UDS の connect は通る)。

**判定: go。** 7 が成立するので、「トークンが読まれても、serve に繋げない」を、kernel の検査で作れる。ただし 8 のため、**檻の中の path の UDS に、トークンを付ける中継を置くと、子がその UDS に繋いで、中継にトークンを付けさせられる** (下の決定 3)。

## 決定

1. **対象は opencode 1.18.x に固定する。** 2.x は対象外 (画面・API が違う)。実測は 1.18.33 (npm の `opencode-ai` の latest も 1.18.33)。起動前に `--version` を見て、1.18 系でなければ起動を拒否する (版が変わると前提が変わる、という claude の教訓。ADR 0017)。版を上げるときは、`cmd/framecapture` の採取を採り直す。
2. **Landlock が使えなければ、起動を拒否する (fail closed)。** `landlock_create_ruleset` の version が 4 未満 (kernel 6.7 未満)・無効 (ENOSYS・EOPNOTSUPP)・ruleset の作成や `restrict_self` が失敗、のどれでも、opencode を起動せず、理由を言って止まる。警告つきで許す経路は作らない。
3. **ホスト ↔ 檻の中の serve の経路は、名前の無い socketpair と `SCM_RIGHTS` にする** (Issue の「UDS を listen する逆方向中継」から変える)。根拠は実測 8: path の UDS は、同じ uid の子が、Landlock でも止められずに connect できる。
   - ホストが control 用の socketpair を作り、片端を init の fd として渡す (ADR 0012 の標準入出力と同じ。init は PID 1・dumpable=0・読めない複製なので、子は `pidfd_getfd`・`/proc/pid/mem` で奪えない)。
   - HTTP の 1 要求ごとに、ホストが新しい socketpair を作り、片端を control 経由の `SCM_RIGHTS` で init に送る。init は、その fd を `127.0.0.1:<serve のポート>` に中継し、**`Authorization` を init が付け直す** (クライアントが付けた `Authorization`・重複・大文字小文字違いは、全部捨てて上書きする)。
   - 檻の中に、path を持つ socket・待ち受けのポートは増えない。子が繋げる入口が無い。
   - 代替 (却下): path の UDS を rw の bind に置き、mode 0600 にする (同じ uid の子は connect できる。中継がトークンを付けるので、トークンを読むより容易)。
4. **トークンは、セッションごとに init が `crypto/rand` で作る使い捨て。** 32 バイトを hex にし、init のメモリと、opencode の環境変数 `OPENCODE_SERVER_PASSWORD` にだけ置く。argv・bwrap の `--setenv`・ホスト側には渡さない (ホストの `/proc/<pid>/cmdline` に出る。ホストは、トークンを知る必要が無い: 3 の中継が付ける)。檻 (セッション) が終われば、プロセスとともに消える。再利用・永続化しない。
5. **子が環境変数を読めても繋げない形にする** (実測 5〜7 の組み合わせ):
   - 起動の前に、Landlock (TCP の connect を、egress の proxy のポートだけに制限。BIND は、最初は制限しない) と `no_new_privs` を掛けてから、opencode を exec する。Go は fork と exec の間でコードを動かせないので、init が、小さな補助のサブコマンド (`goronation landlock-exec --allow-connect <port> -- <exe> <args…>`。Landlock を掛けて `execve` するだけ) を経由して起動する。init 自身には掛けない (init は serve のポートへ繋ぐ必要がある)。
   - opencode は、**読めない複製 (ADR 0012)** から起動する (`environ` を、他プロセスから読めなくする。実測 6)。
   - 子の継承した環境変数にトークンが入るのは、受け入れる: 使えないため (実測 7)。`shell.env` の plugin で消す案は、試していないので採らない。
6. **opencode の承認まわりの設定は、goronation が明示する。** 既定は承認なしで実行される (実測 4) ので、`permission` を `ask` にする設定を渡す。渡し方 (`OPENCODE_CONFIG_CONTENT` の優先順位・project の `opencode.json` の `permission: allow` で迂回されないか) は、**未測**。PR⑥ で、実物で測ってから決める (claude の ADR 0017 決定 6 と同種)。測るまでは、「承認が要求される」とは言わない。
7. **`core/agent.Stream` の port は、行のまま変えない。** SSE と HTTP への写しは、`agent/` の外の transport 層 (`cmd/internal/chat` 側) が持つ。
   - **読み**: transport が SSE の `data:` の JSON 1 行を、そのまま `DecodeFrame` の 1 行として渡す。`agent/opencode` は `net/http` を import しない (archtest の規則を保つ)。session の作成 (`POST /session`) は transport が行い、session ID は、最初の行として合成して渡す。
   - **書き**: `EncodeCommand` は、「HTTP 要求の記述」(`{"method","path","body"}` の JSON 1 行) を返す。transport が、これを init の中継経由で実行する (`prompt_async`・`/permission/{id}/reply`・`/session/{id}/abort`)。
   - 理由: `Stream`・`LineReader`・`Feed`・`Hub` を、claude と共通のまま使える。拡張 (Stream を、SSE event と HTTP の型に広げる) は、port の契約テスト・archtest・claude アダプタまで触る。
8. **`Conversation` は、claude と共通のままにする。** 差は、書く側の `io.Writer` (transport が、有界のキューで順に HTTP を実行する。失敗は `ErrWriteFailed` で会話を終える) と、`OnStop` (`abort` を送り、檻を止める) に閉じる。承認の束縛 (request_id・1 回限り・世代。ADR 0011・0013) は、そのまま使う。**M1.5 では、`allow_always` は出さない** (`once` と `reject` だけ。`always` の `permission.asked.always` は無視する)。
9. **SSE の切断は、会話の終了として扱う (M2 の範囲では、再接続しない)。** 切断・プロセスの終了のどちらでも、未決の権限要求を失効させ、檻を止める (取りこぼし・二重承認を作らない)。SSE の耐久化は、別の課題。
10. **承認の内容ハッシュ束縛・opencode の設定ファイル経由の承認の迂回の全体の調査・`allow_always` は、範囲外。**

## 帰結

- トークンが読まれても使えない。ただし、**守るのは Landlock の kernel 検査に依存する** (決定 2 の fail closed。kernel 6.7 未満では opencode を動かせない)。
- Landlock は、ポート単位で、IP は指定できない。`serve` のポートに繋げない子は、egress の proxy のポートには繋げる (それだけが許可)。opencode が、他のローカルポート (例: MCP のローカルサーバー) へ繋ぐなら、許可に足す必要がある (PR⑥ で、goronation が渡す MCP の設定 (`opencodeMCPInject`: `type: local` の command) は stdio で、ポートを使わないことを確かめる)。
- init の攻撃面が増える (HTTP の要求を、1 本ずつ受けて付け直す)。解釈は最小にする: 要求行・`Authorization` の上書き・長さの上限・接続数の上限だけ。
- 同じ uid の子は、opencode を kill できる (DoS。ADR 0012 と同じで、受け入れる)。
- 新しい fd の経路 (socketpair + `SCM_RIGHTS`) が、子に継承・奪取されないことは、PR③・②の実測 (ptrace_scope=0 の非 root) で確かめる。CI の runner は scope 1 なので、対照は SKIP される (ADR 0012 の運用の約束を守る)。

## 代替案

- **path の UDS の逆方向中継**: 決定 3 のとおり、同じ uid の子が connect できる。却下。
- **トークンをファイルで渡し、読んだら消す**: server 側に、ファイルから読む口が無い (実測 3)。却下。
- **`--password` (クライアント側) だけで済ます**: server 側の認証は、環境変数だけ。却下。
- **起動後に環境変数を消す**: opencode 側にその処理が無い (binary の JS ソースで、起動時に `process.env` を読むだけなのを確かめた)。init が、別プロセスである opencode の環境変数を、後から消す手段も無い (未測だが、そのような手段を前提にしない)。却下。
- **`Stream` を、SSE event・HTTP の型に広げる**: 決定 7。契約・archtest・claude アダプタまで波及する。却下。
- **`Conversation` を、serve 用に別の型にする**: 承認の束縛が 2 系統になり、食い違いの温床になる。却下。
- **`run --format json` を、そのまま使う**: 承認が自動で reject される (実測 2)。対話的な承認が要件なので、却下。`--auto` は、承認を通さず実行するので、論外。
- **警告つきで、Landlock なしを許す**: トークンが読まれると、自分の承認を通せる。claude の `--dangerously-skip-permissions` 相当の運用になる。却下 (決定 2)。

## 未確認 (この ADR が、成立を前提にしているもの)

- Go の init (補助の `landlock-exec`) からの Landlock の適用 (実測は python の `ctypes` で代用した)。
- 0111 の複製と Landlock を、同時に掛けた起動 (別々には測った)。
- host の kernel の Landlock 対応 (この環境は ABI 6)。
- `permission` の設定の優先順位 (決定 6)・`shell.env` plugin・`opencode attach`・`acp`・`always` と reject の後の挙動・v2 API の実挙動。
- socketpair + `SCM_RIGHTS` の経路を、子から奪えないこと (PR②③で実測)。

## PR 分割

| PR | 内容 | 攻撃者視点のレビュー |
|---|---|---|
| ① | この ADR | 不要 (ただし、脅威モデルは、②以降のレビューの入力) |
| ② | 中継: control socketpair・`SCM_RIGHTS`・`Authorization` の付け直し・HTTP の上限・接続数の上限 (`cmd/goronation/init.go`・`relay.go`) | 必須 (ヘッダの上書き・smuggling・fd の奪取・DoS) |
| ③ | トークン (init が生成)・`landlock-exec`・fail closed・読めない複製 | 必須 (「読めても繋げない」を、実際に攻撃して確かめる。赤 → 緑と、再現しなかった攻撃) |
| ④ | `cmd/framecapture` で、serve の HTTP + SSE の golden fixtures を採取 (simple-text・tool-call・permission.asked → allow/deny → replied・複数ターン・エラー・中断) | 不要 (採取の経路が、本番と同じ防御を通ること) |
| ⑤ | `agent/opencode` (アダプタ): 決定 7 の写し。lenient な fail-safe (型が想定と違えば Bad 判定で raw ごと保持) | 必須 (敵対入力の変換・permission の id の偽装・応答の取り違え) |
| ⑥ | `chat.Agent` に opencode を足す・transport・決定 6 の実測 | 必須 (承認の束縛・切断・二重承認・終了時の未決の失効) |
| ⑦ | E2E (fake provider で 1 ターン + 承認) と実物の確認 | 総合の 1 周 |

依存: ① → ②・③ (並行可) → ④ → ⑤ → ⑥ → ⑦。

## 再現手順

前提: `bwrap`・`python3`・`curl`・`ip`・npm の registry への到達。作業は使い捨ての場所で行う。ホストで opencode を実行しない (実行は、檻の中だけ)。

1. `npm pack opencode-linux-x64@1.18.33 --ignore-scripts` → `tar xzf` → `package/bin/opencode`。
2. 檻: `bwrap --unshare-all --die-with-parent [--uid 1000 --gid 1000] --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/lib /lib --symlink usr/lib64 /lib64 --proc /proc --dev /dev --tmpfs /tmp --ro-bind <opencode> /opt/oc --bind <偽 HOME> /home/x --bind <work> /work --chdir /work --clearenv --setenv HOME /home/x --setenv PATH /usr/bin:/bin --setenv OPENCODE_DISABLE_AUTOUPDATE 1 --setenv OPENCODE_DISABLE_MODELS_FETCH 1 --setenv OPENCODE_DISABLE_SHARE 1 --setenv OPENCODE_DISABLE_LSP_DOWNLOAD 1 /bin/bash <script>`。
3. 檻の中の fake: `127.0.0.1:9999` の chat.completions (SSE)。tools が無いリクエストは「タイトル生成」なので、短い文を返す。tools があり、`role: tool` のメッセージがまだ無ければ、`bash` の tool_call を返す。あれば、通常の文。
4. `opencode.json`: `provider.fake` (`npm: @ai-sdk/openai-compatible`・`baseURL: http://127.0.0.1:9999/v1`)・`permission.bash: "ask"`。
5. `OPENCODE_SERVER_PASSWORD=<pw> opencode serve --port 4096` → `GET /event` を `-u opencode:<pw>` で開く → `POST /session` → `POST /session/{id}/prompt_async` (`{"model":{"providerID":"fake","modelID":"fake-model"},"parts":[{"type":"text","text":"go"}]}`、204) → `GET /permission` → `POST /permission/{id}/reply` (`{"reply":"once"}`)。期待: `permission.asked`・`permission.replied` が SSE に流れ、tool が実行される。
6. 子からの到達: bash tool のコマンドを `env | grep OPENCODE_SERVER_PASSWORD`・`curl http://127.0.0.1:4096/global/health` (無認証・`-u opencode:$OPENCODE_SERVER_PASSWORD`) にする。Landlock あり/なしを比べる。
7. Landlock: `landlock_create_ruleset(NULL, 0, 1)` で ABI を取得 → `handled_access_net = CONNECT_TCP (2)` の ruleset → `LANDLOCK_RULE_NET_PORT` で許可するポートを足す → `prctl(PR_SET_NO_NEW_PRIVS)` → `landlock_restrict_self` → `execv` (syscall 番号 x86_64: 444・445・446)。
8. UDS: 同じ手順で、FS の全権 (0xffff) を handled・`/` の読み・実行だけを許可 → `/work/t.sock` (別に listen 済み) へ `connect`。

fixtures は、この ADR では採らない (PR④)。

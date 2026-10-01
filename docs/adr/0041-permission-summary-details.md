# 0041. 権限の要求は、機械生成の要約と、tool に依らない詳細の 2 階層で見せる (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0015・0016・0021・0042、`spec/v0/permission.go`

## 状況

`permission.requested` は、claude の `can_use_tool` の写しで、承認する対象は tool 名と生の `input` (tool ごとに形が違う)。UI は `input` を JSON で出すだけで、コマンドを「コマンド」として見せられない。opencode の要求は `action`・`resources` (許可する対象の一覧) で、生の `input` も自己申告の説明も無い。

## 決定

1. **`permission.requested` に、追加の欄を足す** (既存の欄は変えない。無ければ UI は `input` の表示に戻る): `summary` (1 行・200 字以内・改行なし)・`details` (項目 `{label, text, kind}` の一覧・16 項目以内)・`details_truncated`・`content_hash` (ADR 0042)。
2. **要約と詳細は、承認する対象から、アダプタが機械的に作る。** エージェントの自己申告 (`title`) とは別で、`title` は「検証されていない説明」として別に表示する。`input` は、書き換えず保持する。label は固定の語 (アダプタが決める)。text はエージェントが決めた値で、UI は ADR 0015 の印で表示する。kind は `command`・`path`・`url`・`text`。
3. **詳細の項目は、承認する対象の全体を落とさない。** 1 項目は 4,000 字まで。超えるときは、切って `details_truncated` を立て、UI は承認させない (見せていないものを承認させない。ADR 0016 決定 8 の関門を、input でなく詳細に掛ける)。要約は、切ってよい (詳細が全体を持つ)。
4. **対応の規則** (`spec/v0/conformance_test.go` が、実フレームで固定する):
   - claude: Bash → 要約 `Bash: <command>`・詳細 command。Write → `Write: <file_path>`・path と content。Edit → path・old_string・new_string。Read → path。WebFetch → url・prompt。**知らない tool** → 要約は tool 名・詳細は input の最上位のキーを辞書順に全部 (入れ子は JSON)。
   - opencode: `<action>: <resources を結合>` (shell は ` ; `、ほかは `, `)。詳細は resources を 1 つずつ (shell は command・edit は path・glob と grep は pattern・webfetch は url・websearch は query・ほかは target)。edit は metadata の patch も (`patch: <file>`)。metadata の url・query・root・format も。**shell の部分コマンドは、1 つずつ見せる** (ADR 0021 決定 4)。
   - opencode の `input` は `{action, resources, metadata}` (id・sessionID・source は経路の情報で、request_id・call_id に写す。`save` は always の対象で、v0 は always を出さない)。`kind` は action から (shell → execute・edit と write → edit・read と external_directory → read・glob と grep と websearch → search・webfetch → fetch・ほかは other)。
5. **改行・タブは要約で空白にするが、危険な文字は変えない**: 印にするのは UI の仕事 (同じ入力を、いつも同じ規則で見せる)。

## 帰結

- UI は、要約を一覧に、詳細を承認の画面に出せる。claude と opencode の要求を、同じ画面で扱える。
- 規則はアダプタごとの表で、新しい tool・action は、まず「知らない tool」の規則で落とさず出る。
- 要約と詳細は input から決まるので、内容ハッシュ (ADR 0042) は、これらを含めて、見せた内容を固定する。

## 代替案

- 要約を `title` で済ます: エージェントの自己申告で、偽れる。
- 詳細を生の `input` の JSON のまま: コマンド・path を、種類で見分けられず、承認の画面が tool ごとに要る。

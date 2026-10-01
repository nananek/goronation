# 0021. opencode の承認は、セッション作成時の permissions で固定する (M2)

- 状態: 採用 (2026-10-01 に追記: 決定 5 の `question`・`form` の扱いは、ADR 0040・0043 が置き換える。実装は ADR 0051・0052)
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0011・0017・0018、[PR #68](https://github.com/nananek/goronation/pull/68)

## 状況

claude 2.1.285 は、既定の権限モードが変わり、承認なしで tool が実行された (ADR 0017)。opencode 2.0.20 も、`permission` の設定が無いと、shell が承認なしで実行される (実測)。承認は、SSE の `permission.asked` (`data`: `id`・`sessionID`・`action`・`resources`) に、`POST /api/session/{sid}/permission/{rid}/reply` (`{"decision":"once"|"always"|"reject"}`・204) で返す。`action` は tool 名の `shell` で (設定の `bash` は別名)、`resources` はコマンドを分解した一覧。

設定の優先順位 (実測。shell を 1 回実行させて、承認が要求されたかを見た): repo の `opencode.json` < 環境変数 `OPENCODE_CONFIG_CONTENT` < `POST /api/session` の `permissions` (allow と ask の両方向で、後者が勝った)。`permissions` は、API を呼べる者しか変えられない。

## 決定

1. **`POST /api/session` に `"permissions":[{"action":"*","resource":"*","effect":"ask"}]` を渡す。** repo の設定 (エージェントの子が書ける) でも、環境変数でも、これを覆せない。API は、init の中継 (ADR 0019) を通る要求でしか呼べない。
2. **`OPENCODE_CONFIG_CONTENT` でも `permission: {"*":"ask"}` を渡す** (二重にする)。
3. **返答は `once` と `reject` だけ。** `always` は出さない (保存された許可が、後の承認を飛ばす。内容ハッシュ束縛は ADR 0011 の M2)。承認の束縛 (request_id・1 回限り・世代) は、ADR 0011・0013 のまま使う。
4. **承認の UI は、`resources` を部分コマンドの一覧として見せる。** 1 つでも見えないまま許可させない。
5. **対応しない要求 (`question`・`form` など) は、`agent.frame` として残し、返答しない** (ADR 0012 の L15 と同じ。手動の「終了」で止める)。

## 帰結

- `action` の全一覧と、`execute`・`subagent`・`write`・`edit`・`webfetch` の扱い・複数ルールの評価順序・`always` で保存された許可との関係は未測。**実装の PR で、すべての tool が承認を要求するか、実物で測るまでは、「すべて承認される」と言わない。**
- 版が上がると、設定の優先順位が変わりうる。採取 (ADR 0018) を採り直して確かめる。

## 代替案

- repo の `opencode.json` だけで `ask` にする: 子が書き換えて迂回できる。
- `--auto` を使う: 承認を通さずに実行する。
- `always` を出す: 承認の束縛が弱くなる。

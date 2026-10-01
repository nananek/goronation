# 0052. opencode の session は permissions で作り、設定は 1 つの環境変数にまとめ、-- の引数と resume は限る (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0020・0021・0043・0051、`cmd/goronation/{serve_chat,serve_chat_opencode_linux,mcp,agent}.go`

## 状況

ADR 0021 は承認を session 作成時の `permissions` で固定し、ADR 0043 は `question` の承認の段を省く順序を決めた。ADR 0021 決定 2 は `OPENCODE_CONFIG_CONTENT` の `permission` も渡す (二重) としたが、この名前は MCP の注入 (`opencodeMCPInject`) が既に使う。2 度渡すと後勝ちで片方が消える。

## 決定

1. **`POST /api/session` の本文は `permissions: [{"*" ask}, {"question" allow}]` の順** (あとの規則が勝つ。ADR 0043 決定 2)。定数と試験で順序を固定する。偽の opencode の E2E で、`question` に承認の段が出ない (form が直接出る) こと、shell は `permission.asked` のままであることを確かめる。
2. **環境変数は 1 つにまとめる**: HTTP の transport (RelayPort あり) の `OPENCODE_CONFIG_CONTENT` は `{"$schema", "permission":{"*":"ask"}, "mcp"?}` の 1 つの JSON (MCP の有無によらず permission は必ず入る)。子の session・permissions を持たない経路も、承認を要求する (予備)。`goronation run` の opencode は変えない。実物の opencode で、この予備が session の `question: allow` を打ち消さないかの確認は、手元の E2E の項目 (未確認)。
3. **`relayTokenEnv`・`relayVersionPrefix` は `agentProfile` の欄**で、本番と試験が同じ値を使う (opencode は `OPENCODE_PASSWORD`・`opencode v2.0.`)。ポートは起動のたびに空きを選ぶ (横取りの窓は ADR 0029 の L0〜L3)。
4. **`serve --chat` の `--` 以降の引数は、opencode では断る** (clone の作成より前に)。`--hostname`・`--port`・`--password` の上書きで、待ち受け・認証の前提 (ADR 0020) を崩せないようにする。
5. **`--session` の再開は、新しい opencode session を作る** (限界)。過去の会話は repo ごとの HOME の `opencode.db` に残るが、chat の画面には出ない。一覧・再開は別の PR。

## 帰結

- 承認の段が戻る方向には誤らない。UI が form を描けない版では、question のターンは止まり、Stop で終わる。
- 版が上がって設定の優先順位が変わったら、採取 (ADR 0018) を採り直す。

## 代替案

- 環境変数の `permission` を渡さない: 子の session・予備を失う。実測で打ち消すと分かったら、こちらにする。
- MCP の注入を別の環境変数にする: opencode は設定の環境変数を 1 つしか読まない。

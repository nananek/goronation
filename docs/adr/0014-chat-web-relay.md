# 0014. web の chat の中継: SSE・書き込み・関門・接続の上限 (M1.5)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0009・0013、`cmd/goronation/web_chat.go`

## 状況

serve --chat (ADR 0013) は UDS 専用で、認証もタイムアウトも心拍も無い (Limit)。web が、認証済みのブラウザと serve の間で、SSE と書き込みを中継する。web の `http.Server` は WriteTimeout 30 秒・keep-alive 無効・同時接続 64 で、そのままでは SSE が 30 秒で切れ、枠も食う。

## 決定

1. **ルート** (すべて `requireSession`): `GET /s/{id}/events`、`POST /s/{id}/message`・`/permission`・`/stop`。ページは PR⑦。`{id}` は session の ID の形だけ (path traversal を断つ)。
2. **serve を起こさない**: chat.sock が無い・前の serve の残りで繋がらないときは 404。GET でクレジットを使わない。
3. **書き込みの関門 (CSRF)**: Origin が `--origin` と完全に一致 (無い・null・別は 403)、`Sec-Fetch-Site` があれば same-origin、Content-Type は application/json (415)。SameSite=Strict の cookie に加える二重目。認証 (401) の後。関門・不正な ID・大きすぎる本文 (413) は、serve に届かない。
4. **本文は解析せず、そのまま渡す** (L-G): web は Content-Type・大きさだけ確かめ、検証は serve が 1 か所で行う。重複キー・大文字小文字の違うキー・未知のフィールド・後ろのゴミが、web 経由と serve 直で同じ status・本文になることを、テストで固定する。`generation`・`request_id`・`outcome` は、web が変えず・補わない。応答は、決まった status (200・400・404・409・413・415・500) だけ返し、ほかは 502。
5. **SSE は自前の行コピー**: 1 イベント (data の行と空行) ずつ、書くたびに書き込みの期限を延ばして書き、Flush する。フレーミングを確かめ (行は `event: `・`data: `・空行だけ。1 イベント 4 MiB まで)、破れたらそこで止める。無通信 (既定 15 秒) には `: ping` を書く。クライアントが切れれば、serve への接続も閉じる。
6. **期限と枠 (L-C)**: serve への接続 2 秒・応答のヘッダ 5 秒・書き込み API の全体 10 秒 (超過 504)。SSE の 1 回の書き込み 10 秒 (読まない購読者を切る)。SSE の同時接続は、全体 16・セッション 4 (serve の 32 と web の接続枠 64 より小さい)。超過は 503。
7. **HEAD は serve へ流さない** (405): serve は HEAD でも SSE の枠を使う。

## 帰結

- 応答しない serve は、期限で 504 になり、web の他の要求・枠を塞がない。
- serve の Limit のうち、web が持つべきもの (期限・心拍・枠・method・切断) は、web が持つ。serve 自身のタイムアウトは足さない (信頼境界は UDS)。
- 上流が止まっている (SIGSTOP など) 間、確立済みの SSE は、心拍だけを送り続ける (無通信の切断は、静かな会話と区別できないので持たない)。
- 世代の束縛 (UI が、ダイアログを見せた時点の世代を送る) は、PR⑦b。

## Limit

- 上流の SSE の行の途中に裸の `\r` があっても、web は通す (ブラウザの EventSource は `\r` を行の区切りにする)。いまは、serve が `\r`・`\n` を含む data を捨てるので、到達しない。web の枠組みの検査に `\r` の拒否を足すと整合する。
- 心拍だけでは、消えた相手 (FIN が来ない) を、書き込みの期限では検知できない。TCP keepalive (既定で有効) に頼る (解放までの時間は未計測)。その間は、セッションの枠を握る。
- Origin・Sec-Fetch-Site が複数あるときは、最初の値だけを見る (ブラウザは複数を送れない)。`Content-Type: application/json;text/plain` も通る (serve と共通の判定)。応答が 64 KiB を超えると、黙って切る (serve の応答は小さい)。

## 代替案

- `httputil.ReverseProxy` で SSE を中継する: 書き込みの期限・心拍・フレーミングの確認を、差し込めない。
- web で本文を検証してから中継する: 別の解析器との食い違い (L-G)。

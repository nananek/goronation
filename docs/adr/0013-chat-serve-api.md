# 0013. serve --chat の UDS API と、承認の世代 (M1.5)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0009・0011・0012、`cmd/goronation/serve_chat_http.go`

## 状況

PR④ までで、檻の stream-json を会話 (Hub・Conversation) に流せる。PR⑤ で、`goronation serve --chat` に、UDS の HTTP API を付ける。起動をまたぐ承認の誤適用 (ADR 0012 の L12: 起動 1 の古い画面の allow が、起動 2 の同じ request_id の別の input に通る) を、API の形で断つ。

## 決定

1. **UDS は `chat.sock`** (`termSocketPath` の兄弟。0600・ロック・前回の残りの掃除は term.sock と同じ)。認証は無い (UDS に繋がれること自体が信頼境界)。起動完了の行は、端末ビューと同じ (`goronation serve: <path> で待ち受けている (session=<id>)`)。起動しただけでは、エージェントに何も送らない。
2. **`GET /events` (SSE)**: `event: hello` (`{"first_seq","generation"}`) → バッファ全部 → ライブ → `event: end` (`{"exit":N}`)。`data:` は Event の JSON 1 行 (JSON は改行・CR をエスケープする。含んでいたら捨てる)。上限 32 本で超過は 503、1 回の書き込みは 30 秒で切る。遅くて外された購読者は、`end` なしで閉じる (繋ぎ直すと、バッファは全部届く)。
3. **書き込み**: `POST /message {text}`・`POST /permission {generation, request_id, outcome}`・`POST /stop`。本文は Content-Type: application/json・上限つき・未知のフィールドと後ろの余分な値は 400。状態の食い違い (ターン中・終了後・応答済み・別の起動) は 409、未知の request_id は 404、大きすぎるは 413。
4. **世代 (案 1)**: `hello` に `generation` (起動ごとのランダム値) を載せ、**`/permission` は世代が必須** (欠落 400・不一致 409。未決の表には触れない)。spec/v0 は変えない。各 `permission.requested` に世代を焼き付ける案 2 は、M2 の内容ハッシュ束縛と合わせて検討する。
5. **UI が守ること (PR⑦b)**: 世代は、接続の最新の hello ではなく、**ダイアログを見せた時点の世代**に束縛する。hello の世代が変わったら (serve の再起動で EventSource が再接続した)、表示中のダイアログを全部無効化する。API は、世代を起動の間だけ不変にし (永続化しない)、hello を接続ごとに返すことで、これを守れる形にしてある。
6. **世代なしの経路は型で無い**: `Conversation.Resolve` は非公開 (`resolve`)。公開は `ResolveIn` だけで、世代が空なら `ErrNoGeneration`。
7. **`serve --chat` は root では動かさない**: 何も作る前に、理由を言って断る (ADR 0012 決定 3)。手動の確認は非 root で行う。
8. **読めない複製の作成・掃除 (ADR 0012 の L1)** は、dir のロックで直列にし、使うたびに更新時刻を今にして、最後に使われてから 10 分過ぎた古い複製だけ消す (版の違う実体の並行起動で、他方の path を消さない)。

## 帰結

- 世代が守るのは、起動をまたぐ誤適用だけ。同じ起動の中の request_id は、Stream が一意にし、応答は 1 回限り (ADR 0011)。内容ハッシュは M2。
- 古い画面が世代を持ったまま、同じ serve に繋ぎ直しても、世代は同じなので通る (同じ起動の要求だから、L12 ではない)。
- エージェントの標準エラー出力が ready 行に一致しないことを、配線の後も、結合テストで固定する (ADR 0012)。

## Limit

- `http.Server` に、読み書きのタイムアウトが無い。放置した接続・SSE 32 本で枠を塞げる。信頼境界は UDS で、接続の枠・期限・心拍は web (PR⑥) が持つ (PR⑥ のレビューの対象)。
- 読めない複製の lock (`flock` の待ち) は、シグナルで中断できず、lock の持ち主が止まると起動が待つ。
- 起動の順序: セッション・檻が、UDS の path の検査・lock より先に作られる。`--socket` の既存の通常ファイルは、消されてソケットに置き換わる (端末ビューから継承)。
- root の拒否のテストは root でだけ動く (CI は非 root で skip)。root でも別に回す。
- 「厳格な JSON」は Go の既定: 重複キーは後が勝ち、キー名の大文字小文字は区別しない。web (PR⑥) は、同じ `decodeStrict` を使うか、本文を解析せず中継する。

## 代替案

- 世代を任意にする: 古い画面 (世代を送らない) が通り、L12 が残る。
- 世代を `permission.requested` に載せる: 標準形式・ADR 0010 の変更が要る (M2)。

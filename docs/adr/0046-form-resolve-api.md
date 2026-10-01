# 0046. form の応答の API (POST /form) と、承認の応答の content_hash (M2)

- 状態: 採用 (2026-10-01: 決定 6 を実施 (必須にした。乗っ取りには効かない))
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0013・0016・0040・0042・0044・0047、`cmd/goronation/serve_chat_http.go`

## 状況

form (ADR 0040) への回答を、画面から返す経路が無い。claude の AskUserQuestion は、回答が届かず、質問が機能していない (ADR 0044)。承認の応答 (`POST /permission`) は、利用者が見た内容と結べていない (ADR 0042)。

## 決定

1. **`POST /form` を足す** (serve の UDS。web は `POST /s/{id}/form` で、本文を解析せず中継する。ADR 0013 の L-G)。本文は `{"generation","request_id","outcome":"answered"|"cancelled","answer"?:{"<key>":"<文字列>"|["<文字列>",…]},"content_hash"?}`。`generation`・`request_id`・`outcome` は必須。`answer` の値は、文字列か文字列の配列だけ (数・真偽・null・入れ子は 400 `bad_json`)。
2. **`POST /permission` に、任意の `content_hash` を足す** (見た要求の `content_hash` の写し)。本文は、どちらも、厳格な JSON: 未知のフィールド・後ろの余分な値に加え、**同じキーが 2 つあるオブジェクト (最上位は、Go の JSON の読みと同じ simple fold で同一視。`ſ` は `s`) は 400 `bad_json`** (後の値が勝つ読みで、検証と実行が食い違わないように)。web と serve は、同じ本文に同じ応答を返す。
3. **応答のコード** (permission と共通): 200 `{"ok":true}`・400 `bad_request` (outcome・世代なし)・**400 `bad_answer`** (回答が、保持した要求に合わない)・**400 `content_hash_required`**・404 `unknown_request` (種類の違う要求の ID も)・409 `already_resolved`・`stale_generation`・**409 `content_changed`**・413・415。拒否は、何も書かず、未決のまま。
4. **回答は、保持した form に対してだけ検査する** (`FormResolve.Validate`。ADR 0040 決定 6)。クライアントの値が、エージェントに渡るのは、この検査を通ったものだけ。エージェントには `{request_id, outcome, answer}` だけを渡し、`content_hash` は渡さない。
5. **本文の上限は、変えない**: `chatMaxBody` (約 394 KB) は、16 フィールド × 4,000 字 × エスケープ 6 倍に足りる (test で固定)。1 つの multiselect に、長い値を大量に入れる回答 (正当でも) は 413 になりうる。
6. **`content_hash` の必須化は、別の判断**: 既定では、無くても通る (あれば必ず照合する)。M2 の本実装で必須にするのは、UI が写すようになってから (ADR 0042 決定 3)。

## 帰結

- UI (PR⑤b-2) は、`form.requested` の `content_hash` を、回答・承認に写して送れる。
- 回答の内容は、`form.resolved` の `answer` として、同じ SSE の全購読者にも見える (回答は、エージェントと、画面を開く人に見える。秘密の入力は促さない)。
- ADR 0013 の `POST /permission` の本文に `content_hash` が、API の一覧に `POST /form` が加わる。

## 代替案

- `POST /permission` に form の回答も載せる: 承認 (許可・拒否) と入力 (値) は、結果の形も検査も違う (ADR 0040 の代替案)。
- 重複キーを、後が勝つまま通す: web と serve で解析が分かれると、検証と実行が食い違う。

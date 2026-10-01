# 0047. 内容ハッシュは Feed が付け、Conversation が照合する (M2)

- 状態: 採用 (2026-10-01: 必須化は実施済み。ADR 0042)
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0040・0041・0042・0046、`cmd/internal/chat/{feed,conversation}.go`

## 状況

ADR 0042 は、ハッシュを共通の層が付け、応答が写し、保持した値と一致しなければ拒否する、と決めた。どの層が付け、どこで照合し、検査に通らない要求をどう扱うかは、実装の決定として残った。

## 決定

1. **付けるのは `Feed`** (アダプタと Conversation の間)。`permission.requested`・`form.requested` の `data` を、`spec/v0` の型に読み、`Validate()` を通れば、`content_hash` を `Hash()` で入れて、`data` を書き直す。**アダプタが付けた `content_hash` は、上書きする**。型に無い欄は、ここで落ちる (UI・API に出る欄は、語彙の欄だけ)。
2. **検査に通らない要求は、`agent.frame` (data 空・durable でない) にして配る**: 読めない・`Validate` に通らない・大きすぎる要求を、承認も回答もできない形にする (アダプタの検査をすり抜けた形の、二重の防御)。要約・詳細が無い旧い形は通る (後方互換)。
3. **保持するのは `Conversation`**: 未決の要求ごとに、配った Event・`content_hash`・(form は) パースした `FormRequested` を持つ。ハッシュは、アダプタの値でなく、**配った Event の値**。
4. **照合は `Conversation`** (`ResolvePermissionIn`・`ResolveFormIn`): 世代 → 未決か (種類が合うか) → `content_hash` (`VerifyHash`。違えば `ErrContentChanged`・無くて必須なら `ErrContentHashRequired`) → form は回答の検査 (`ErrBadAnswer`) → エージェントへ書く。**アダプタは、ハッシュを知らない** (`Command.Data` に入れない)。権限と form は、同じ未決の表・同じ上限 (`MaxPendingRequests`) で持ち、**種類の違う要求の ID への応答は、未決でないものとして拒否する** (form に `permission.resolve` の allow を返すと、回答なしの許可になる)。
5. **`ConversationConfig.RequireContentHash`** (既定 false): false の間は、無ければ照合せず、あれば必ず照合する。true は、ハッシュのない応答を拒否する。
6. **form も、失効・取り消しの規則は権限と同じ**: 上限超過・ターンの外・Stop・終了・書き込みの失敗は、by=policy・cancelled の `form.resolved` で決着させ (エージェントへは `form.resolve` cancelled)、未決の form は Hub に固定する。状態名 `awaiting_permission` は、権限または form を待つ意味で、変えない。

## 帰結

- 守るのは、事故 (古い画面・別の要求の取り違え) まで (ADR 0042 決定 5)。ハッシュは同じ SSE で読めるので、セッションを乗っ取った者は写して通せる。
- ハッシュは、要約・詳細の作り方 (アダプタの更新) で変わる。要求の間だけ有効。
- 必須化 (`RequireContentHash`) は、UI が写すようになってから、別の判断で行う。

## 代替案

- アダプタが付ける: 付け忘れ・誤りが、アダプタごとに起きる (ADR 0042 決定 2)。
- 検査に通らない要求を、error で捨てる: 何も配られず、承認する画面が無いまま、エージェントが待ち続けるのが、見えない。

# 0024. chat の SSE 再接続: `after=seq`・`generation`・Hub の境目 (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0008・0013・0015・0023・0025

## 状況

- 繋ぎ直しは、毎回リングの全部を再送する (ADR 0013 決定 2)。溢れた分は「省略」としか示せない。ADR 0008 は、再接続を `after=seq` と決めている。
- UI は、既に `seq <= lastSeq` の重複捨てと `first_seq > lastSeq + 1` の省略検出を持つ (ADR 0015)。EventSource を作り直す自前の再接続なので、`Last-Event-ID` ヘッダは付かない。
- web の中継 (`readSSEEvents`) は、`event: `・`data: `・空行以外の行で止める。

## 決定

1. **`GET /events?after=N&generation=G`** (web 経由の `/s/{id}/events` も同じ。web が値を検証して渡す)。`generation` が現在の世代と一致し、`after` が 10 進数の非負整数 (2^53 未満) のときだけ、`seq > N` の durable から再送する。**一致しない・欠ける・不正な値は、エラーにせず、今までどおり全部 (pinned + 直近) を送る。**
2. `after` が保持している最初の `seq` より前なら、保持している分から送る。hello の `first_seq` が示し、UI の既存の省略検出が働く。
3. **未決の権限要求は、`after` と保持の範囲に依らず、再接続で届く。** 読み取りは `SELECT payload FROM events WHERE session_id=? AND generation=? AND seq>? AND seq<? ORDER BY seq` (上の `<` は、リングの先頭) で、**`pinned=1` の行は GC で消えない** (ADR 0023 決定 6) ので、`after < 未決の要求の seq < 保持の先頭` でも、同じ SELECT で、`seq` 順に返る。重複は UI の `seq <= lastSeq` の捨てで防ぐ。
4. **Hub**: `Subscribe()` は変えず、`SubscribeAfter(after)` を足す。同じ Mutex の中で、リングの先頭・購読者の登録を取り、DB から `seq` がリングの先頭より前の行を、ロックの外で読み、リング、ライブの順に続ける。境目に取りこぼしも重複も無い。読み取りは `LIMIT` つきの `seq` の続きから、何回かに分ける (1 回ごとの短い読み取りで、GC や追記と競合しない)。`Conversation` の `Publish`/`Update`/`pin` の意味は変えない。
5. **SSE**: 各イベントに `id: <seq>` の行を足す (`Last-Event-ID` ヘッダも、クエリより弱い扱いで受ける)。`id:` の値は 10 進数だけ。**`Last-Event-ID` だけで `generation` が無い場合は、世代を確かめられないので、無視して全再送する。** web の中継が許す行に `id: ` を足すが、値が 10 進数以外なら、今までどおり止める。
6. `hello` に `resumed` (`after` が効いたか)・`durable` (ADR 0025) を足す。UI は未知の欄を無視する。
7. **UI の変更は数行**: 再接続の URL に `lastSeq` と世代を付けることと、`resumed` のとき省略の印を出さないことだけ (OpenCode 対応の UI 変更と同じファイルなので、最小にする)。
8. **遅い購読者**: DB から読んでいる間にライブのキューが溢れると、今までどおり外される。外された側は進んだ `after` で繋ぎ直すので、1 回ごとに前進する。
9. **実装 PR への申し送り**: (a) DB 全体の上限があるので、セッション数に対する合計は DB 側で数えられる (ADR 0023 決定 6)。(b) web の `readSSEEvents` の許可リストの変更は、注入の攻撃面として、実装 PR の攻撃者視点レビューの対象にする。

## 帰結

- 得るもの: 差分だけの再接続と、リングを超える履歴。`after` を持たない接続は、今までどおり動く。
- 新しい攻撃面: `after`・`generation`・`id:` の注入 (web と serve の両方で厳格に検証する)、`after` の連打で DB を読ませ続ける DoS (SSE の接続の枠の中で、読む量に上限を置く)。
- 世代が違う `after` を信じると別の起動の履歴に繋がるので、一致を必ず確かめる。
- ADR 0013 決定 2 (「バッファ全部」を送る) は、実装が入った時点で、この ADR に従う。

## 代替案

- **`Last-Event-ID` ヘッダだけ**: 自前の再接続 (EventSource の作り直し) では付かない。クエリを主にする。
- **世代の不一致をエラーにする**: 古い画面が繋がらなくなる。全再送に倒せば、UI は世代の変化で表示を作り直す (ADR 0013)。
- **`pinned` を別のクエリ・前置きで送る**: SELECT の 1 本で `seq` 順に返せば足りる。

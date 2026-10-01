# 0053. 耐久イベントログは、同期の Store (eventlog) と、chat の非同期の書き手に分ける (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0023・0024・0025・0027、`cmd/internal/chat`・`cmd/internal/eventlog` (実装の PR)

## 状況

ADR 0023 決定 7 は、実装を `cmd/internal/eventlog` に置き、境界のインターフェースを `chat` に置く、とした。ADR 0027 は、書き込みを `Hub` の Mutex の外の専用 goroutine で行い、縮退・書き込み済みの最大の `seq` (W)・量の上限を持つ、とした。この 2 つを、どちらの package が持つかは決めていない。`chat` は I/O を持たない規則 (doc.go) で、`modernc.org/sqlite` を import してよいのは `eventlog` だけ (ADR 0026)。

## 決定

1. **`eventlog` は、同期の Store だけを持つ。** 基本型 (`seq`・JSON のバイト列) の API: `Apply` (追記・固定・固定の解除を 1 トランザクション)・`Range(after, before, limit)` (読み取り専用の接続)・`FirstUnpinned`・`Trim(budget)` (`pinned=0` の古い行から削る)・`Size`・`Close(remove)`・起動時の `SweepStale`。キュー・縮退・W は持たない。`chat` を import しない。
2. **非同期の書き手は `chat` が持つ。** `Hub.Update` は、Hub が実際に決めた結果 (固定できたもの・解除したもの・durable の Event) を、有界のキューに積むだけ。専用の goroutine が `Apply` し、W・縮退の判定 (ADR 0027 決定 5)・`Trim` の起動を持つ。縮退は、コールバックで呼び手 (serve) に知らせる (`chat` は標準エラー出力に書かない)。
3. **インターフェース (`EventStore`) は `chat` に置く。** `eventlog` の型が満たす。型は、`chat` の側にも同じ形で持ち、`cmd/goronation` が薄く繋ぐ (`chat` が `eventlog` を import しない)。
4. **ロックは `eventlog` が、DB と同じディレクトリの `events.lock` で持つ** (ADR 0023 決定 5)。`Close(remove)` が、DB・`-wal`・`-shm`・`events.lock` を消す。
5. **`Store` から読んだ行は、信用しない。** `chat` が、JSON として妥当か・大きさ・`seq` の単調増加を確かめてから配る (同じ利用者の別プロセスが、DB を書き換えうる)。

## 帰結

- `eventlog` の実装 (SQLite) と `Hub` の統合を、このインターフェースだけで、並行に実装・試験できる。`Hub` の試験は、遅延・失敗を注入できる偽の `Store` で、ADR 0027 の完了条件 2・4 を確かめられる。
- 型の定義が 2 か所になる (薄い変換の保守)。`chat` が I/O を持たない規則と、依存の例外の範囲 (ADR 0026) は、保たれる。
- 書き手の縮退の判定が `chat` にあるので、`Store` の遅延は、`Hub` の Mutex の保持時間に及ばない (完了条件 2)。

## 代替案

- **`eventlog` が書き手 (キュー・goroutine) も持つ**: `Hub` が `eventlog` を import するか、`Hub` が書き手のインターフェースを持つことになり、境目の不変条件 (W と押し出し) を `Hub` の外から守る形になる。
- **`chat` が SQLite を直接使う**: `chat` が I/O を持ち、依存の例外が `chat` に広がる (ADR 0026 は `eventlog` に限る)。
- **`Store` を非同期の API にする**: 試験が難しくなり、書き込み済みの位置 (W) の持ち主が曖昧になる。

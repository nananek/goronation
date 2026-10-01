# 0054. 耐久イベントログの配線: serve が開いて閉じ、events が after で続け、web が検証して中継する (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0023・0024・0025・0027・0053

## 状況

ADR 0053 は、Store (eventlog) と書き手・SubscribeAfter (chat) を分けた。この ADR は、それを `goronation serve --chat` と `goronation web` に繋ぐときの決定を置く。実コードを読んで、計画の誤りが 4 つ見つかった (決定 1〜4)。

## 決定

1. **DB の置き場所は、`--socket` と無関係に、`<state>/groups/default/sessions/<id>/`** (`termSocketPath` の dirname)。`--socket` で UDS を動かしても、DB は掃除の走査範囲 (`sessions/*/`) の中にあり、残りが掃除される。ディレクトリは 0700 で作る。
2. **Store の世代は、承認の世代 (`Conversation.Generation()`) とは別の、呼び手が作るランダムなログ ID** (`crypto/rand` 16 バイトの 16 進)。Store は `NewSession` の前に開く必要があるが、承認の世代は `NewSession` の中で作られるため。ログ ID は DB の中だけの区別で、外に出さない。`after` と照合する世代は、承認の世代のまま。
3. **`eventlog.Open` は、セッション ID が決まった直後・檻を起こす前**。`ErrLocked` (別の serve が同じセッションの DB を持つ) は、起動の失敗。それ以外の失敗・`SweepStale` の掃除のあと全セッションの DB が 256 MiB 以上 (ADR 0027 決定 7) のときは、標準エラー出力に 1 回出して、耐久化せずに続ける (`durable=false`)。`SweepStale` は、serve の起動ごとに 1 回。
4. **DB の削除は、`chatSession.run` ではなく、serve プロセスの終了の経路**: エージェントが終わっても、serve は動き続け、画面は再接続できる (ADR 0024 決定 4)。HTTP の shutdown・檻と会話の終了のあと、`Hub.WaitStore` (5 秒まで) → `Close(remove=true)`。`kill -9` の残りは、次の起動の掃除が消す。
5. **`OnDegrade` の reason は、Store の error (path を含みうる) を含む**ので、無害化 (制御文字・改行を除く) して、運用者の標準エラー出力にだけ出す。画面・SSE には、error の文字列を出さない。
6. **events**: クエリ `after` (10 進数の非負整数・16 桁まで・2^53 未満) と `generation` (現在の承認の世代と完全に一致)・どちらもキーが 1 つだけのときだけ `SubscribeAfter`。それ以外 (`ErrBadAfter` を含む) は、エラーにせず全再送。**`Last-Event-ID` は実装しない** (ADR 0024 決定 5): 世代を確かめられず、自前の再接続では付かない。hello に `resumed`・`durable` を足す。送る順は hello → Backfill → Snapshot → ライブ。各 Event に `id: <seq>`。Backfill の失敗 (stale・読めない・不正な行) は、何も足さずに閉じる。
7. **1 接続の Backfill は、16 MiB まで**。超えたら閉じ、画面は進んだ `after` で繋ぎ直す (1 回ごとに前進する)。同時接続数 (32) と書き込みの期限は、そのまま。
8. **web は、クエリを検証して、`url.Values` で作り直す**: `after` = `^[0-9]{1,16}$`・`generation` = `^[0-9a-f]{32}$`。`RawQuery` はそのまま渡さない。不正・重複は、クエリなし (全再送)。`readSSEEvents` が許す行に、`^id: [0-9]{1,16}$` を足す (1 イベントに 1 行)。
9. **フラグは `--no-event-log` だけ**。量 (起動あたり 64 MiB・合計 256 MiB・キュー・Backfill) を変えるフラグは、必要になるまで足さない (ADR 0023 決定 6 の「フラグ名は実装の PR で確定」の答え)。

## 帰結

- ディスクに残る内容 (tool の入出力・本文・form の回答・承認の対象) は、0600・暗号化なしで、進行中の起動の間だけ。usage に書く。
- 異常終了の残りは、次の serve の起動まで残る。serve が二度と起動されなければ、残り続ける。
- hello の `first_seq` は、GC が削った範囲の上端の次 (最後の `Trim` の返り値)。after のすぐ後が非 durable の欠番で、after が削った範囲の境目にいると、省略の印が余計に出うる (非 durable は、元から再送されない。害なし)。
- UI (④b) が `after` を付けるまでは、画面は全再送のまま (害なし)。

## 代替案

- **DB を `filepath.Dir(sockPath)` に置く**: `--socket` のとき、掃除の外に DB が残る。
- **Store の世代に承認の世代を使う**: `NewSession` の前後で順序が逆になる。承認の束縛の設計を動かす。
- **`run` の終わりで DB を消す**: エージェントの終了後の再接続が、履歴を失う。

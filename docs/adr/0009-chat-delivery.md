# 0009. 構造化チャットの配信方式 (M1.5): SSE・メモリのリングバッファ・serve --chat

- 状態: 採用
- 日付: 2026-09-29
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (M1.5)、ADR 0008・0010、`cmd/internal/chat` の doc (規則と限界)

## 状況

M1.5 は、封筒を人間に見せ、指示と承認を返す。`goronation web` は WriteTimeout=30s・同時接続 64 で、エージェントの出力は敵対入力。`Envelope` は Raw (path・設定などを含みうる) を持つ。

## 決定

1. **配信は SSE** (`data:` は `Envelope.Public()` の JSON 1 行。終了は `event: end` の `{"exit":N}`)。耐久ストアと `after=seq` の再接続は M2。M1.5 は、serve のメモリに**バイト上限つきのリングバッファ** (既定 4 MiB) を持ち、接続時に全部送ってからライブにする。`event: hello` の `first_seq` が 0 でなければ、溢れて省略した分がある (seq は 0 から)。再読み込みで未決の権限要求が復元できるのは、バッファに残る間だけ。リングはバイトの上限なので、エージェントが大量に出力すれば、未決の要求も押し出せる。押し出しても失効させない保持は、状態機械 (未決の表) が持つ。
2. **serve に `--chat`** (UDS 専用・認証なし。信頼境界は UDS に繋げること自体)。`chat.sock` は、term.sock と同じディレクトリ。**GET で claude を起こさない** (`chat.sock` が無ければ 404)。
3. **1 行の上限は 1 MiB**。未決の権限要求は input を 1 行分保持し、agent/claude の未決の上限 (64) との積 (約 64 MiB) が最悪のメモリ量になる。超える行は捨てる (tool の入力が 1 MiB を超える要求は、承認できない)。Event の JSON は 2 MiB まで (超えたら捨てる)。`<` `>` `&` は JSON でエスケープしない (6 倍に膨らみ、1 行で購読者を外し、履歴を押し出せたため。JSON は SSE の `data:` と `JSON.parse` だけに使う)。購読者のキューは、空なら 1 件は必ず受ける。
4. **`spec/v0` の import 元を、`agent/**`・`core/**`・`spec/**`・`cmd/internal/chat/**` に限る** (archtest の `v0-only-in-chat`)。web は Event の JSON と小さな型付きの要求だけを見る。`Public()` の約束を、規則で強制する。`Stream.DecodeFrame` の戻り値の型は推論で決まり、spec/v0 を import しなくても `Raw` に届くので、`core/agent` と `agent/**` (実装。将来の追加分も) の import も同じ範囲に限る (`agent-only-in-chat`)。
5. **簡易な承認の束縛** (書く側。別の PR): 要求 ID に束縛・1 回限り・許可する input は保持した値だけ・claude の終了で失効・同じ ID の再要求は捨てる・未決の上限。内容ハッシュの束縛 (設計ルール 10) は M2。
6. **止めるのは手動の「終了」だけ。** タイムアウトによる自動停止は作らない。承認待ちのまま放置した chat と、押し忘れた「終了」で、檻が残るリスクは受け入れる。自動停止は将来の検討事項 (実装しない)。
7. **名前** (reversibility-check を通した。採用後は、web の起動・テスト・doc に散るので、変えるには数ファイルの変更):
   - フラグ `--chat`。却下: `--structured` (長い)・`--stream` (pty との違いが伝わらない)。
   - ソケット `chat.sock`。却下: `events.sock` (書き込みも通る)・`agent.sock` (エージェントのものに見える)。
   - URL `/s/{id}/chat` (ページ)・`/events` (SSE)・`/message`・`/permission`・`/stop`。

## 帰結

- M2 で耐久ストアに置き換えるとき、`Hub` を差し替える。SSE のフレーミングと `hello` の形は保つ。
- 承認フローの完全性は、標準入出力の transport (ADR 0010) に依存する。この配信の部品は、出力が本物のフレームであることを、確かめられない。

## 代替案

- WebSocket: 依存 (ADR 0005 の例外) を広げる。書く側は POST で足りる。
- 耐久ストア (SQLite など) を最初から: 依存ゼロの方針に反し、M1.5 の範囲を超える。
- `httputil.ReverseProxy` で SSE を中継: WriteTimeout で 30 秒で切れ、購読者を外せない。自前の行コピーにする (web の PR)。
- タイムアウトの自動停止: 承認待ちの chat を勝手に殺す。見送る (決定 6)。

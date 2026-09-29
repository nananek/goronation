# 0015. チャット画面の表示 (読む側) (M1.5)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0009・0013・0014、`cmd/goronation/assets/chat/`

## 状況

web の中継 (ADR 0014) の上に、構造化チャットの画面を作る。この版は表示だけ (送信・承認は PR⑦b)。エージェントの出力は敵対入力で、画面に出るものは、`<script>`・双方向の上書き・巨大な文字列・大量のイベントでありうる。

## 決定

1. **ルート**: `GET /s/{id}/chat` (requireSession。形の悪い ID は 400。serve には触れない)、`/static/chat.js`・`chat-core.js`・`chat.css`。素のファイル (`assets/chat/`) を embed する。インラインの script・style・イベントハンドラは無い (CSP は既存のまま)。セッション一覧の各行に `[チャット]` のリンクを足す (開始の入口は PR⑧)。
2. **描画は textContent と createElement だけ**。`innerHTML`・`outerHTML`・`insertAdjacentHTML`・`document.write`・`eval`・`new Function`・href/src の代入・on* の代入などを、chat.js・chat-core.js が含まないことを、Go のテストが (コメントを除いて) 検査する。検査自体が効くことも、禁止の各 API の見本で確かめる。
3. **危険な文字は見える印にする**: C0・DEL・C1・双方向制御 (U+202A〜202E・2066〜2069・LRM・RLM・ALM)・行/段落区切り・ゼロ幅・BOM・タグ文字・対になっていないサロゲートを `<U+202E>` の形にする。ハングルの埋め字・点字の空白・軟ハイフン・物体の置換文字なども印にする。**短い項目** (名前・title・ID・状態) は、さらに空白に見える文字・改行・タブ・異体字選択子も印にする (名前が空白に見えない・ラベルの中に行を偽造できない)。長い本文は、全角空白・絵文字の選択子を残す。CSS は `unicode-bidi: isolate`・`white-space: pre-wrap`・`overflow-wrap: anywhere`。
4. **長さを切る**: 発言 20,000 文字・input/出力 4,000・名前や title 300 (切ったら、全体の長さを示す)。項目は約 1,500 (まとめて捨てる。未決の権限要求は捨てない)。描画は 50ms ごとにまとめ、触れた項目だけ更新する。
5. **表示する type**: session.started・turn.started (送った文)・message.text・tool.call/update (call_id で 1 枠)・permission.requested/resolved・usage・turn.completed・error。agent.frame・未知の type・不正な形は、数えて無視する。要求の無い permission.resolved は無視する。**未決の上限 (64) は、決着していない要求の数だけに効く** (決着済みが枠を食わない)。`outcome` は許可リスト (allow_once・reject_once・cancelled。ほかは unknown で、未決に見せない)。tool.update の呼び出しが省略されていても落ちない。
6. **再接続**: seq が既出なら捨てる (同じ世代の Snapshot の重複)。**世代の無い・32 桁の 16 進でない hello は受けない** (その接続を閉じ、間隔を空けて繋ぎ直す。世代は承認に使う)。hello の世代が変われば、表示を全部作り直す (serve の再起動で seq が振り直される)。`hello.first_seq > 0`・繋ぎ直しの欠けは「古い分は省略」。`end` で接続を閉じ、再接続しない。切断は、EventSource の自動再接続に任せず、自前で間隔を倍々に空ける (1〜30 秒)。hello に届かない失敗が 10 回続いたら諦め、ボタンで再開する (chat が起動していない・503・404 は、status が見えない)。`: ping` は EventSource が届けない。
7. **権限要求は、表示だけ**: 未決は「表示だけ」と出し、ボタンは無い。input を切ったかは `cut` に持つ (PR⑦b は、切ったものは Approve を無効にする)。世代の束縛は PR⑦b。
8. **試験**: 表示ロジックは純関数 (chat-core.js) に分け、node の `--test` で、偽の DOM・EventSource の上で試験する (golden を本物の変換器に通した Event、敵対的な文字列、30 万イベント、再接続、世代の変更)。CI は `GORO_REQUIRE_NODE=1`。

## 帰結

- 実ブラウザでの見た目・EventSource の再接続の実挙動は、自動試験では確かめていない (偽の EventSource)。PR⑧ の E2E・手動確認で見る。
- 危険な文字の印は、表示の偽装を減らすが、見た目の紛らわしい文字 (同形異字) までは防がない。承認の画面 (PR⑦b) は、tool 名・input を構造化イベントからだけ描く。

## Limit (記録のみ)

- hello の直後に上流が切れ続けると、hello で間隔が戻るので、1 秒ごとの再接続が続く (自分のセッションの枠だけ)。項目の更新で、開いた `<details>` が閉じる。
- 静的な禁止 API の検査は、動的な参照 (`el['inner'+'HTML']`・`setHTMLUnsafe`・`location = x` など) を見逃す。将来の編集への守りで、エージェントの出力が実行時の sink に入る経路は見つかっていない (実 Firefox で確認)。承認の UI (PR⑦b) は、この検査を信じない。
- 1 MiB の Event を続けると CPU を使う (描画はまとめるので固まらない)。同形異字は防がない。

## 代替案

- innerHTML で描き、エスケープする: 1 か所の漏れで DOM 注入になる。
- markdown を描画する: スコープ外 (M2)。
- 再接続を EventSource に任せる: 503・素の EOF で、間隔が空かず、繋ぎ続ける。

# 0017. チャットの開始の入口・E2E・権限モード (M1.5)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0010・0012・0014・0016、`cmd/goronation/web_chat.go`、`cmd/internal/chat/agent.go`

## 状況

M1.5 の最後の PR。Web から chat を開始する入口と、web → serve → 檻 → エージェントを通した確認を加える。実物の claude の確認で、承認の経路そのものが、claude の版で無効になりうることが分かった。

## 決定

1. **`POST /api/chat/start {repo}`** (requireSession): 書き込みの関門 (Origin・Sec-Fetch-Site・Content-Type。ADR 0014) を通し、`--repos-dir` 直下の repo を `goronation serve --chat --repo` で起こして `{id}` を返す。最初の指示はチャット欄から送る (起動しただけでは何も送らず、クレジットを使わない)。端末ビューの `/api/repos/start` は変えない。
2. **二重起動の防ぎ**: 同じ repo の起動待ちは 1 つだけ (二重クリック・二重送信は 409)。全体の起動待ちは 4 まで (超過 503)。画面のボタンも、押したら両方の開始ボタンを無効にする。
3. **L-D の対処**: UDS の path が長いと、serve はセッションと檻を作ってから失敗していた。web は起こす前に、serve は何も作る前に、path の長さを確かめる (ID は同じ長さの仮のもの)。root で動かした web の失敗は、「root では動かせない」と分かる文言で返す (serve の出力はそのまま返さない)。
4. **一覧**: `/api/sessions` に `chat` (chat.sock がソケットとして在るか) を足し、`[チャット]` のリンクは、在るときだけ出す。repo 一覧は「端末で開始」「チャットで開始」の 2 つ。
5. **権限モードを明示する**: claude 2.1.285 は、既定の権限モードが `auto` に変わり、`--permission-prompt-tool stdio` を付けても `can_use_tool` の要求が出ず、Write が**人間の承認なしで実行された** (2.1.284 は `default`)。起動引数に `--permission-mode default` を足した (実物で、許可で実行・拒否で実行されないことを確認。設定の `defaultMode` にも勝つ)。
6. **設定ファイル経由の迂回** (実測。2.1.285・`default` のまま): 承認なしで実行されるのは、repo の `.claude/settings.local.json` の allow、repo の `.claude/settings.json` の hooks (`PreToolUse` が allow)、HOME の `~/.claude/settings.json` の allow・hooks。repo の `.claude/settings.json` の allow は効かない。書き換えは次の起動から効く。HOME は repo ごとに永続し、檻から書ける。`--setting-sources user` を足した: project・local 由来 (2 つ) は切れる。**HOME 由来は切れず、残る** (HOME を読み取り専用にする案は未測定)。**managed settings の allow も、`--setting-sources user` では切れず、迂回になる** (N-M1)。書けるのは host の root だけで、檻の中の攻撃者の経路ではないので、Blocking にしない。
7. **検出とその限界**: アダプタは `session.started` に `permission_mode` を載せる。画面は、`default` 以外、または、無い・空 (確認できない) なら、「人間の承認なしで実行されうる」と警告する。**警告は `permission_mode` の値だけを見るので、6 の迂回 (値は `default` のまま) は検出できない。** 強制 (自動で止める) はしない: 運営者が `--` の後ろで別のモードを選べる。
8. **E2E** (bwrap・非 root): 実際の web のハンドラ → web が起こす実プロセスの serve --chat → 実際の檻 → 偽の claude (許可された tool を実際に実行する)。開始 → 指示 → 権限要求 → 世代の検査 → 再読み込みでの未決の復元 → 許可 (new.txt ができる) → 拒否 (できない・拒否が伝わる) → 終了。
9. **V8 の深さ**: node 22 (V8 12.4) で、`JSON.stringify` は深さ 4500 で RangeError、`JSON.parse` は 20000 まで通る。`boundedJSON` は、どの深さでも打ち切る (ADR 0016)。

## 帰結

- E2E は偽の claude。実物の 1 ターンを serve 経由で通すには、認証か fake provider への経路 (serve に無い) が要る。確認の範囲は PR 本文に書く。
- `--permission-mode default` は、claude の版が変わっても効く保証は無い。警告は、その検出の最後の砦。
- HOME の設定と、Bash などの副作用で承認を迂回する経路は、塞がない (ADR 0012・決定 6)。

## 代替案

- 権限モードが default でなければ、自動で止める: 意図した別のモードを使えなくなる。警告に留めた。
- E2E に fake provider を使う: serve にテスト専用の口が要る。

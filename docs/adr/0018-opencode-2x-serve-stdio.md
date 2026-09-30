# 0018. opencode は 2.x の serve --stdio で起動する (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0012・0019・0020・0021・0022、[PR #68](https://github.com/nananek/goronation/pull/68) (実測と再現手順)

## 状況

opencode の対話的な権限承認は、HTTP + SSE のサーバーにだけある (`run` は承認を自動で reject する。`--auto` は承認なしで実行する)。最初は作業環境にあった 1.18.33 (npm の `opencode-ai`) で測ったが、最新は 2.x で、別の配布物 (npm の `@opencode/cli`・2.0.20。GitHub Releases には無く、タグだけ) だった。2.x は API・tool 名・承認の設定がすべて違う。

2.x の `serve --stdio` は、標準入出力で API を話す方式ではない。TCP の HTTP サーバーのままで、標準入出力は制御にだけ使う (実測: stdout は `{"url":"http://127.0.0.1:4096"}` の 1 行だけ・`/proc/net/tcp` に 127.0.0.1:4096 の LISTEN・unix socket の LISTEN は 0・API はすべて TCP・stdin に余計なバイトを書いても動作は変わらない。ソースは stdin の end と close だけを見る)。よって Issue #1 の「`serve` の TCP リッスンを受け入れる」決定は、そのまま成り立つ。

## 決定

1. **対象は opencode 2.0.x に固定する。** 1.x は対象外。起動前に `--version` が `opencode v2.0.` で始まるかを見て、違えば起動を拒否する。2.1 以降は、採取を採り直してから範囲を広げる。
2. **起動は `opencode serve --stdio --hostname 127.0.0.1 --port <空きポート>`。** init が opencode の標準入力に pipe を繋いで保持する。init が死ぬと stdin が閉じ、サーバーも終わる (孤児が残らない)。標準出力の `{"url":…}` 1 行で、起動完了とポートを確かめる。
3. **背景サービスを残さない。** `run`・TUI・`mini`・`api` は、既定で背景サービス (`serve --service`) に繋ぎ、終了後もそれが残る (実測)。`--standalone` を付ければ残らない。goronation がこれらを起動するときは必ず `--standalone` を付ける。`serve` には `--standalone` が無く、前景のサーバーなので要らない。chat は `serve --stdio` だけを使い、起動引数は `chat.Agent` が固定する。
4. **背景サービスは使わない。** サービスの設定にパスワードが保存され、檻の後始末・使い捨てのトークン (ADR 0020) に反する。

## 帰結

- 版が上がると、API・tool 名・承認の設定が変わりうる (1.18 → 2.x で全部変わった)。版の検査と、フレームの採り直しが、版を上げる手順になる。
- `opencodeProfile` (`goronation run` 用) は 1.18.32 の実測のまま。2.x 対応の要否は、M2 の最後の PR で決める。

## 代替案

- 1.18.x に固定する: 最新ではない。2.x と API が違い、後で作り直しになる。
- `run --format json` を使う: 承認を対話的に返す口が無い。
- 背景サービスに繋ぐ: 決定 4。

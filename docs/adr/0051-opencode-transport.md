# 0051. opencode の transport は、状態の 1 行を読み、SSE を先に開き、root の一致を確かめてから会話を渡す (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0019・0020・0022・0028・0029・0030・0049・0050、`cmd/goronation/serve_chat_opencode_linux.go`、`cmd/internal/chat/{http,sse}.go`

## 状況

ADR 0022 は、opencode の HTTP と SSE を transport 層に置いた。檻の中の opencode (`serve --stdio`) は、標準入力が control (init が子を起こし、ホストが要求ごとに socketpair の fd を送る)、標準出力が init の状態の 1 行 (ADR 0030)。出力は敵対入力で、ホストが読んで会話に流し、会話の指示を HTTP にして送る境界が、新しい攻撃面になる。

## 決定

1. **起動の順序**: `startCage` → init の状態の 1 行 → SSE (`GET /api/event`) を開く → `POST /api/session` → root の一致 → 会話。SSE を session の作成より先に開く (後だと、root の `session.created` を取りこぼす)。
2. **状態の行**: 上限 4 KiB・60 秒。`{"ready":true}` だけが成功。`{"error":…}` は無害化して失敗 (init が返した失敗は、ポートを選び直して 1 回だけ再起動)。未知の欄・EOF・時間切れは失敗。**この行を読み終えるまで、control に何も送らない。** 子の標準出力は init の pipe で、ホストへの socketpair には届かない (子が `{"ready":true}` を書いても、`/proc/1/fd/1` を開こうとしても、起動は成功しない。試験で確認)。
3. **SSE**: 最初の `data:` が `server.connected` (10 秒)。`data:` 以外の行は読み飛ばし、行の上限・NUL・不正な UTF-8 は `LineReader` の検査を通す。複数の `data:` 行に分かれたイベントは、行ごとに別の本文になり、JSON として読めない側は捨てられる。
4. **root の一致**: `POST /api/session` が返した ID と、SSE の最初の parentID の無い `session.created` の sessionID が一致するまで、会話に何も渡さない (root の前の行は捨てる)。不一致・10 秒以内に来ない場合は、起動の失敗 (fail closed。ほかの利用者の session を root にしない)。会話は、一致を確かめた後にだけ返る。
5. **書き込み (`httpWriter`)**: アダプタの出す 1 行を、`chat.ParseHTTPRequest` が独立に検査する (厳密な JSON・method と path の固定の形 5 種・ID は `[A-Za-z0-9_-]`・body は object か無し・1 MiB)。許可外は実行しない。30 秒の期限。**2xx 以外は error** で、書き込みの失敗として会話が終わる (承認の返答が失敗したのに、承認済みのように続かない)。
6. **終わり**: SSE の EOF・error は、再接続せず、檻を止める (ADR 0022 決定 5)。手動の終了は、control を閉じて檻を止める。

## 帰結

- 新しい待ち受けは増えない (HTTP は control 越し)。待ち受けの時間・同時接続の制限は対象外。
- 偽の opencode (golden を再生) で、承認・form・子の session・Stop・檻の死・敵対的な起動を、実物の無い CI で通す。実物の opencode での確認は、手元・夜間。

## 代替案

- SSE を session の作成の後に開く: root の `session.created` を取りこぼす。
- root の不一致を、その行だけ捨てて続ける: 別の利用者の session が、後から root になりうる。

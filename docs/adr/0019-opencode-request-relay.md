# 0019. opencode サーバーへの通信は、socketpair と SCM_RIGHTS の中継で通す (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0012・0018・0020、`cmd/goronation/init.go`・`relay.go`、[PR #68](https://github.com/nananek/goronation/pull/68)

## 状況

opencode のサーバー (ADR 0018) は、檻の中の loopback の TCP で待ち受ける。ホストの serve は、これに HTTP で繋ぐ必要がある。Issue #1 の案は、檻の中に path の UDS を置き、init が UDS から 127.0.0.1 へ中継して `Authorization` を付ける、というものだった。

実測: Landlock (ABI 6) は、path の UDS への connect を止められない。FS の全権を handled にして `/` の読み・実行だけ許可しても、`connect` は成功した。kernel の性質で、opencode の版には依らない。

## 決定

1. **檻の中に、path を持つ socket・待ち受けのポートを増やさない。** 同じ uid の子 (エージェントの tool) が繋げる入口を作らない。
2. **ホストが control 用の socketpair を作り、片端を init の fd として渡す** (ADR 0012 の標準入出力と同じ。init は PID 1・dumpable=0・読めない複製なので、子は `pidfd_getfd`・`/proc/pid/mem` で奪えない)。
3. **HTTP の 1 要求ごとに、ホストが新しい socketpair を作り、片端を control 経由の `SCM_RIGHTS` で init に送る。** init は、その fd を `127.0.0.1:<ポート>` に中継する。
4. **`Authorization` は init が付け直す。** クライアントが付けたもの (重複・大文字小文字違いを含む) は全部捨てて、init が持つトークン (ADR 0020) で上書きする。ホストは、トークンを知らない。
5. **init の HTTP の解釈は最小にする。** 要求行・`Authorization` の上書き・長さの上限・接続数の上限だけ。

## 帰結

- init の攻撃面が増える。ヘッダの上書き・smuggling・fd の奪取・DoS は、中継の PR の攻撃者視点のレビューで必ず確かめる。
- fd の経路が、子に継承・奪取されないことは、ptrace_scope=0 の非 root での実測で確かめる (CI の runner は scope 1 で、対照は SKIP される。ADR 0012 の運用の約束)。

## 代替案

- path の UDS を rw の bind に置き、mode 0600 にする: 同じ uid の子が connect できる。中継がトークンを付けるので、トークンを読むより容易に、承認を通せる。
- トークンを付けずに中継する: サーバーが無認証になり、子が直接繋げる。

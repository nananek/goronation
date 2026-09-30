# 0020. opencode のトークンは使い捨てにし、読めても繋げない 3 重の守りを置く (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0012・0018・0019、`cmd/goronation/init.go`、[PR #68](https://github.com/nananek/goronation/pull/68) (実測と再現手順)

## 状況

opencode サーバーの認証は Basic で、パスワードは環境変数 `OPENCODE_PASSWORD` でしか渡せない。檻の中の子がこれを得ると、自分の承認を通せる。実測 (2.0.20): 環境変数のままだと opencode の `/proc/<pid>/environ` は同じ uid に読める。`serve --stdio` は、パスワードの環境変数を自分で消して、tool の子に継承させない。

Landlock の TCP connect 制限は、通常の TCP の connect だけを止める。**MPTCP のソケット (`IPPROTO_MPTCP`) の connect と、`MSG_FASTOPEN` の `sendto`・`sendmsg` は、許可外のポートに繋がった** (kernel 6.12・ABI 6。攻撃者視点のレビューと、手元の再現で一致。通常の TCP・`TCP_FASTOPEN_CONNECT` は EACCES)。**SCTP・UDP も止まらない** (SCTP のモジュールが入っているホストで、許可外ポートの SCTP listener にデータが届いた。UDP の sendto も届いた。到達先は同じ netns の loopback だけ。PR #72 の攻撃者視点レビュー)。path の UDS も止まらない (ADR 0019)。

## 決定

1. **トークンは、セッションごとに init が `crypto/rand` で作る使い捨て。** 32 バイトを、init のメモリと opencode の環境変数にだけ置く。argv・bwrap の `--setenv`・ホスト側には渡さない。檻が終われば消える。
2. **子が、トークンに届かず、サーバーにも繋げない形を、3 重にする。**
   - `serve --stdio` による、環境変数の継承の断ち (opencode の挙動で、版に依存する。テストで確かめる)。
   - opencode は、読めない複製 (mode 0111。ADR 0012) から起動する。`environ` を他プロセスから読めなくする。
   - 補助のサブコマンド `goronation landlock-exec --allow-connect <port> -- <exe> …` が、Landlock (TCP connect を egress の proxy のポートだけに制限) と **seccomp** (下の 3) と `no_new_privs` を掛けて `execve` する。opencode とその子孫だけに掛け、init には掛けない。
3. **seccomp で、Landlock が止めない TCP の経路を塞ぐ。** `socket` は許可リストにする: `AF_UNIX`・`AF_NETLINK` は通し、`AF_INET`・`AF_INET6` は TCP (`SOCK_STREAM`・protocol 0 か 6) だけ通す。MPTCP・SCTP・DCCP・UDP・UDP-Lite・raw・`AF_VSOCK`・`AF_SMC` など、それ以外は `EPERM`。`sendto`・`sendmsg`・`sendmmsg` の flags に `MSG_FASTOPEN`、`io_uring_setup`・`enter`・`register` を `EPERM` にする (io_uring は seccomp を通らず、socket の作成や送信ができるため、予防として塞ぐ)。x86_64 以外の ABI・x32 の syscall も `EPERM`。**実機 (非 root の bwrap の檻) で、MPTCP・SCTP・UDP・MSG_FASTOPEN が EPERM になることを確認した。実物の 2.0.20 が、init・`landlock-exec`・この許可リストの下の非 root の bwrap の檻で、起動し、承認つきの 1 ターン (shell の実行・provider への通信は proxy 経由) を終えることも確かめた (PR③c の `TestRealOpencodeInCage`)。**
4. **Landlock か seccomp が使えなければ、起動を拒否する (fail closed)。** Landlock の ABI が 4 未満 (kernel 6.7 未満)・無効、seccomp の設定や ruleset の作成・適用の失敗のどれでも、opencode を起動せず理由を言う。

## 帰結

- 3 つ目は kernel に依存する。x86_64 以外 (arm64 など) は、syscall 番号と arch を足すまで動かさない (fail closed)。
- socket 以外 (`sendto` の flags・io_uring) は拒否の一覧で、未知の経路は塞げない。新しい経路が見つかったら足す。Landlock の新しい ABI が MPTCP を扱うなら、見直す。
- UDP を止めても、opencode 2.0.20 は動く (DNS は proxy が行う。上の実測)。接続済みの fd を渡されれば、制限の外で使える (Unix の性質。檻には渡し元が無い)。
- Landlock はポート単位で IP は指定できない。opencode が proxy 以外のローカルポートに繋ぐなら、許可に足す。
- 同じ uid の子は opencode を kill できる (DoS。受け入れる)。

## 代替案

- 檻の netns で `net.mptcp.enabled`・`net.ipv4.tcp_fastopen` を 0 にする: 書けるのは root だけ (非 root の uid 1000 は Permission denied と実測。root の chat は動かさない。ADR 0012)。
- ADR の宣言を「MPTCP・TFO・SCTP・UDP は対象外」と書き直すだけにする: 子が許可外のポートに繋げる状態が残る。
- `--stdio` だけ・Landlock だけに頼る: 版依存・`environ` が読める・kernel の検査 1 つが破れる。
- トークンをファイルで渡す・警告つきで Landlock なしを許す: サーバーに口が無い・承認を通される恐れが残る。

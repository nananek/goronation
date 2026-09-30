# 0020. opencode のトークンは使い捨てにし、読めても繋げない 3 重の守りを置く (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0012・0018・0019、`cmd/goronation/init.go`、[PR #68](https://github.com/nananek/goronation/pull/68)

## 状況

opencode サーバーの認証は Basic (ユーザー名 `opencode`) で、パスワードは環境変数 `OPENCODE_PASSWORD` でしか渡せない (設定ファイル・引数・stdin の口は無い)。檻の中の子がこれを得ると、自分の承認を通せる。実測 (2.0.20): 環境変数のままだと opencode の `/proc/<pid>/environ` は同じ uid に読める。`serve --stdio` は、パスワードの環境変数を自分で消して、tool の子に継承させない (`--stdio` でない serve は継承した)。Landlock の TCP connect 制限を掛けた opencode の子は、サーバーのポートに繋げない。

## 決定

1. **トークンは、セッションごとに init が `crypto/rand` で作る使い捨て。** 32 バイトを、init のメモリと opencode の環境変数にだけ置く。argv・bwrap の `--setenv`・ホスト側には渡さない (ホストの `/proc/<pid>/cmdline` に出る)。檻が終われば消える。再利用・永続化しない。
2. **子が、トークンに届かず、サーバーにも繋げない形を、3 重にする。**
   - `serve --stdio` による、環境変数の継承の断ち (opencode の挙動で、版に依存する。テストで確かめる)。
   - opencode は、読めない複製 (mode 0111。ADR 0012) から起動する。`/proc/<pid>/environ` を他プロセスから読めなくする。
   - 起動の前に、Landlock の TCP connect を egress の proxy のポートだけに制限し、`no_new_privs` を掛ける。掛けるのは opencode とその子孫で、init には掛けない (init はサーバーに繋ぐ)。Go は fork と exec の間でコードを動かせないので、補助のサブコマンド (`goronation landlock-exec --allow-connect <port> -- <exe> …`。Landlock を掛けて `execve` するだけ) を経由する。
3. **Landlock が使えなければ、起動を拒否する (fail closed)。** `landlock_create_ruleset` の version が 4 未満 (kernel 6.7 未満)・無効・ruleset の作成や `restrict_self` の失敗のどれでも、opencode を起動せず理由を言う。警告つきで許す経路は作らない。

## 帰結

- 2 と 3 は kernel の検査に依存する。kernel 6.7 未満では opencode を動かせない。
- Landlock はポート単位で IP は指定できない。opencode が egress の proxy 以外のローカルポート (ローカルの MCP など) に繋ぐなら、許可に足す。
- 同じ uid の子は opencode を kill できる (DoS。受け入れる)。

## 代替案

- `--stdio` だけに頼る: 環境変数の削除は opencode の実装の挙動で、版が変わると消えうる。`environ` も残る。
- Landlock だけに頼る: トークンが `environ` で読めたまま。kernel の検査 1 つが破れると、承認を通される。
- トークンをファイルで渡し、読んだら消す: サーバー側に、ファイルから読む口が無い。
- 警告つきで Landlock なしを許す: 承認を通される恐れを、運用に残す。

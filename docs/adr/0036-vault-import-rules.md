# 0036. `vault/` を import してよいのは `cmd/**` だけで、`vault/` は `core/` と標準ライブラリだけに依存する (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0001・0004・0031・0032、`tools/archtest/rules.go`

## 状況

`tools/archtest/rules.go` は、`dep-core` の禁止リストに `vault/**` を既に挙げ、`vault/` は「未作成でも書いてある」。Vault は、全ての秘密を持つので、import できる場所と、依存できる範囲を、作る前に固定する。

## 決定

1. **`vault/` は、新しいトップ module にする** (ADR 0001 決定 1。module path は `github.com/nananek/goronation/vault`。`go.work` に足す)。port は `core/vault` (ADR 0032)、実装は `vault/`。
2. **`vault/**` を import してよいのは、`cmd/**` だけ。** `impl-only-from-cmd` の対象 (`sandbox/bwrap/**`・`agent/claude/**` など) に `vault/**` を足す。egress・agent・sandbox・hostfs・core は、`core` の port (ADR 0032) だけを使う。
3. **`vault/**` は、`core/**` と標準ライブラリしか import しない。** 新しい規則 `dep-vault` は、許可リスト方式にする: `vault/**` (`_test.go` も) が import してよいのは、標準ライブラリと `core/**`・`vault/**` だけ。禁止リスト (`agent/**`・`sandbox/**` など) にしないのは、外部 module (cbor・websocket・sqlite など。`cmd` の require で解決できる) や `spec/**` も、止めるため。
4. **`dep-core` は、変えない** (`core` は `vault/**` を import できない。ADR 0032 の port は `core/vault` で、`vault/` の実装を知らない)。
5. **`vault/**` は、プロセスを起動しない。** `execAllowed` (`sandbox/**`・`cmd/**`) に入れないので、`exec-import`・`exec-call`・`unsafe-import` などが、そのまま効く。gpg を動かす檻は、`sandbox/` と `cmd/` の側にある (ADR 0031 決定 4)。
6. **外部の module は、入れない。** 依存ゼロの例外は、ADR 0004 (WebAuthn)・0005 (WebSocket)・0026 (SQLite) の場所に限られ、`vault/` に無い。WebAuthn の解析は `cmd/internal/webauthn` が行い、`cmd/` が、PRF 出力のバイト列だけを `vault/` に渡す。`vault/` は WebAuthn の型を知らない。
7. **規則は、`vault/` を作る PR (PR③) で実装した。** `tools/archtest/rules.go` の表: `impl-only-from-cmd` に `vault/**` を足す・`OnlyImports` の `dep-vault`・`NoRequires` の `vault-no-require` (`vault/go.mod` は `require` も `tool` も持たない。workspace では、他の module の require で解決できてしまう)。fixture は `dep-vault`・`vault-import`・`vault-no-require`。

## 帰結

- 秘密を扱うコードは、`vault/` の 1 か所に集まり、呼べる場所が `cmd/**` に限られる。egress が Vault を直接 import する形は、作れない。
- `cmd/**` は、`vault/` の全ての操作を呼べる。呼ぶのは、Vault のデーモン (`goronation vault serve`。ADR 0037) と、`goronation vault` の限られた場所だけにする (レビューの対象)。
- `impl-only-from-cmd` は、実装自身の側の import も違反にするので、`vault/` の下の package は、互いに import できない (単一の package で作る)。

## 代替案

- `cmd/internal/vault` に置く: `cmd/` の他のコードから、import の規則で分けられない。egress が使う port と、実装が、同じ module に入る。
- `vault/` が `hostfs/` を使ってよいことにする: `vault/` は、檻が書いたファイルを、path で開かない (読むのは `cmd/` で、`hostfs` を通し、名前と内容のバイト列だけを渡す)。渡された内容 (ログイン状態。ADR 0032) は、敵対入力として、名前・大きさ・数を検査して保管する。

# 0036. `vault/` を import してよいのは `cmd/**` だけで、`vault/` は `core/` と標準ライブラリだけに依存する (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0001・0004・0031・0032、`tools/archtest/rules.go`

## 状況

`tools/archtest/rules.go` は、`dep-core` の禁止リストに `vault/**` を既に挙げ、`vault/` は「未作成でも書いてある」。Vault は、全ての秘密を持つので、import できる場所と、依存できる範囲を、作る前に固定する。

## 決定

1. **`vault/` は、新しいトップ module にする** (ADR 0001 決定 1。module path は `github.com/nananek/goronation/vault`。`go.work` に足す)。port は `core/vault` (ADR 0032)、実装は `vault/`。
2. **`vault/**` を import してよいのは、`cmd/**` だけ。** `impl-only-from-cmd` の対象 (`sandbox/bwrap/**`・`agent/claude/**` など) に `vault/**` を足す。egress・agent・sandbox・hostfs・core は、`core` の port (ADR 0032) だけを使う。
3. **`vault/**` は、`core/**` と標準ライブラリしか import しない。** 新しい規則 `dep-vault` (`ForbidImports`) で、`vault/**` から `agent/**`・`sandbox/**`・`egress/**`・`hostfs/**`・`gateway/**`・`control/**`・`cmd/**` への import を禁じる。
4. **`dep-core` は、変えない** (`core` は `vault/**` を import できない。ADR 0032 の port は `core/vault` で、`vault/` の実装を知らない)。
5. **`vault/**` は、プロセスを起動しない。** `execAllowed` (`sandbox/**`・`cmd/**`) に入れないので、`exec-import`・`exec-call`・`unsafe-import` などが、そのまま効く。gpg を動かす檻は、`sandbox/` と `cmd/` の側にある (ADR 0031 決定 4)。
6. **外部の module は、入れない。** 依存ゼロの例外は、ADR 0004 (WebAuthn)・0005 (WebSocket)・0026 (SQLite) の場所に限られ、`vault/` に無い。WebAuthn の解析は `cmd/internal/webauthn` が行い、`cmd/` が、PRF 出力のバイト列だけを `vault/` に渡す。`vault/` は WebAuthn の型を知らない。
7. **規則の実装は、`vault/` を作る PR で、`tools/archtest/rules.go` の表を変えて行う。** 表の変更に対する、規則の fixture (違反の見本) も、その PR で足す。`vault/go.mod` の `require` を空に保つ検査の形も、その PR が決める。

## 帰結

- 秘密を扱うコードは、`vault/` の 1 か所に集まり、呼べる場所が `cmd/**` に限られる。egress が Vault を直接 import する形は、作れない。
- `cmd/**` は、`vault/` の全ての操作 (解錠・passkey の追加・reset) を呼べる。`cmd/**` の中でも、呼ぶのは、`goronation web`・`goronation vault`・`goronation serve` の、限られた場所にする (レビューの対象)。
- `vault/` 用の新規則を足すまで、この決定は、archtest では強制されない (`vault/` は未作成)。

## 代替案

- `cmd/internal/vault` に置く: `cmd/` の他のコードから、import の規則で分けられない。egress が使う port と、実装が、同じ module に入る。
- `vault/` が `hostfs/` を使ってよいことにする: `hostfs` は、檻が書いたファイルを読む部品で、Vault の保管は、檻の入力を読まない。

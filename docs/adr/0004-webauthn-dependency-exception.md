# 0004. WebAuthn の署名検証・CBOR デコードに限り、依存ゼロの方針の例外を認める

- 状態: 採用
- 日付: 2026-09-28
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (Web UI は WebAuthn 必須)、`core/credential` (`doc.go` に「将来の Vault」への差し替えを見越した記述がある)

## 状況

- このリポジトリの全 module (`cmd`・`core`・`egress`・`hostfs`・`sandbox`・`tools/archtest`・`tools/docgen`) の `go.mod` に、`require` ブロックが 1 つも無い。標準ライブラリだけで書く、という強い既存の規約が、これまで例外なく守られてきた。
- `goro serve` の Web ログイン (WebAuthn によるセッション認証) を実装するには、次の 2 つが要る。
  1. **attestation/assertion オブジェクトの CBOR デコード** (WebAuthn の仕様上、認証器の応答は CBOR で符号化される)。Go 標準ライブラリに CBOR の実装は無い (JSON はあるが CBOR は無い)。
  2. **公開鍵の署名検証** (ES256・RS256 など)。これ自体は `crypto/ecdsa`・`crypto/rsa`・`crypto/ed25519` で標準ライブラリだけで足りる。
- CBOR パーサを自前で書くことは可能だが、他の「檻が書ける入力を、ホストが読む」経路 (`hostfs`・`egress/git` など) と違い、WebAuthn は **認証そのもの** を通すコードで、パースのバグが認証バイパスに直結しやすい。自前実装のテスト網羅より、実績があり fuzz されている既存実装の方が、この 1 点に限れば安全側に倒せると判断した。

## 決定

**WebAuthn の検証コード (署名検証・CBOR デコード) に限り**、信頼できる既存の Go ライブラリを使ってよい、という、依存ゼロの方針 (これまでの全 module に共通の規約) の例外を認める。

- 例外の範囲は、WebAuthn の登録・認証セレモニーの検証ロジックとその直接の依存だけに閉じる。それ以外のコード (`sandbox`・`egress`・`hostfs`・`cmd/goro` の既存部分など) は、これまでどおり標準ライブラリだけで書く。
- 具体的にどの module がこの例外の対象になるかは、`goro serve` の WebAuthn 実装 PR (この ADR の対象外。まだ着手していない) で決める。対象 module を、既存の zero-dependency な module から分離しておくこと (1 つの module が依存を持つと、`go.work` 全体でなく、その module の `go.sum` だけに閉じる)。
- `tools/archtest` に、「この特定の package・module 以外は `go.sum` を持たない (= 依存を追加しない)」ことを機械的に検査するルールを、WebAuthn 実装 PR で追加する。現行の `os/exec` を `sandbox/**`・`cmd/**` に限定するルールと同じ形 (import 制約テスト) で表現できる見込み。

### ライブラリの候補と比較 (調査結果。最終選定は WebAuthn 実装 PR で行う)

2026-09-28 時点で調査した候補:

| 候補 | 直接依存 | 主なライセンス | 特徴 |
|---|---|---|---|
| A. `fxamacker/cbor/v2` (CBOR デコードだけ) + 自前のセレモニー実装 | `x448/float16` の 1 つだけ (どちらも MIT) | MIT | CBOR デコードだけを外部に任せ、challenge の生成・clientDataJSON の検査・署名検証の組み立ては自分で書く。依存の footprint が最小。ただし、セレモニーのロジック自体 (どのフィールドをどう検査するか) の正しさは、こちらの責任になる。 |
| B. `go-webauthn/webauthn` (RP ライブラリ一式) | `fxamacker/cbor/v2`・`go-webauthn/x`・`golang-jwt/jwt/v5`・`google/go-tpm`・`google/uuid`・`go-viper/mapstructure/v2`・`tinylib/msgp` など、直接だけで 9 個、間接も含めるとさらに増える (2026-09-28 に `go.mod` を確認)。BSD-3-Clause | セレモニー全体 (登録・認証・attestation の検証方針) を任せられ、実績もある。ただし pre-1.0 (v0.18.2。破壊的変更がありうる) で、goro には要らない機能 (TPM/Android の企業向け attestation・JWT) まで依存に付いてくる。 |

**この ADR の時点での見立て (仮の推奨。決定はしない)**: goro serve は、企業向けの attestation ポリシー (TPM 等) を要求する想定ではなく、少人数・自分用のツールなので、依存の footprint が小さい **候補 A** に傾く。ただし、セレモニーのロジックを自前で書く以上、そのテスト (特に、署名検証を通すべきでない入力を通さないことを確かめる、否定的なテスト) を厚くする必要がある。最終判断は、実装に着手する PR で、実際のブラウザ (WebAuthn API) との結合テストを踏まえて行う。

## 帰結

- `go.work` の中に、`go.sum` を持つ module が初めて生まれる (候補 A・B のどちらでも)。CI の `fmt-check`/`vet`/`test`/`build` は、`go.mod` のある module ごとに回る既存の仕組み (`Makefile` の `each_module`) なので、新しい module を `go.work` の `use` に足す作業と、依存の脆弱性検査 (`go list -m all` や `govulncheck` など) を CI に足すかどうかの検討が、WebAuthn 実装 PR の作業に含まれる。
- `tools/archtest` に、依存を持ってよい module/package を限定するルールを足す作業が、WebAuthn 実装 PR の作業に含まれる (この ADR では実装しない)。
- 依存の更新 (CVE 対応・API の破壊的変更への追随) という、これまで発生しなかった種類の保守作業が、対象 module にだけ生じる。
- 依存ゼロの方針そのものは、この 1 点以外では変えない。他の機能追加を理由に、この例外を広げない (新しく依存を足したくなったら、この ADR とは別に、その都度、判断と記録を残す)。

## 代替案

検討して却下した案:

- **CBOR も自前実装する (依存ゼロを完全に維持する)**: 認証バイパスに直結しうるパースコードを、実績のある fuzz 済み実装より自分で書く方が安全、とは判断しなかった。
- **WebAuthn 自体を諦め、パスワード等の別の認証方式にする**: Issue #1 で「Web UI は WebAuthn 必須」とすでに決まっており、この ADR の対象外 (ユーザーの決定を覆す提案はしない)。
- **依存ゼロの方針を、リポジトリ全体で撤回する**: 今回の必要は WebAuthn の検証コードに限られており、他のコード (檻・egress・hostfs など、敵対入力を直接扱う部分) まで依存を許す理由は無い。範囲を絞った例外にとどめる。

# 0004. WebAuthn の署名検証・CBOR デコードに限り、依存ゼロの方針の例外を認める

- 状態: 採用
- 日付: 2026-09-28 (2026-09-28 に追記: ライブラリの選定を確定し、`tools/archtest` のルールを実装した)
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (Web UI は WebAuthn 必須)、`cmd/internal/webauthn` (この決定の実装)

## 状況

- 全 module (`cmd`・`core`・`egress`・`hostfs`・`sandbox`・`tools/*`) の `go.mod` に `require` が無い、という強い既存の規約が、これまで例外なく守られてきた。
- `goronation serve` の Web ログイン (WebAuthn) には、(1) attestation/assertion の CBOR デコード (標準ライブラリに CBOR は無い)、(2) 公開鍵の署名検証 (`crypto/ecdsa` 等で標準ライブラリだけで足りる) が要る。
- CBOR を自前実装することは可能だが、WebAuthn は認証そのものを通すコードで、パースのバグが認証バイパスに直結しやすい。自前より、実績があり fuzz されている既存実装の方が、この 1 点に限れば安全側に倒せると判断した。

## 決定

**WebAuthn の検証コード (署名検証・CBOR デコード) に限り**、信頼できる既存の Go ライブラリを使ってよい、という、依存ゼロの方針の例外を認める。

- 例外の範囲は、WebAuthn の登録・認証セレモニーの検証ロジックとその直接の依存だけ。他のコード (`sandbox`・`egress`・`hostfs`・`cmd/goronation` の既存部分など) は、これまでどおり標準ライブラリだけで書く。
- 対象は、新しい top-level module ではなく、既存の `cmd` module の下の `cmd/internal/webauthn` (結合テスト用の `cmd/internal/webauthn/webauthntest` を含む) にする。`goronation serve` は単一の `goronation` バイナリのサブコマンドで (Issue #1「単一バイナリで配布」)、`cmd/goronation` が静的にリンクする以上、`cmd` module の `go.sum` が cbor の checksum を持つことは module を分けても避けられない。分けて守れるのは「他の 4 module が依存ゼロのままであること」で、それは `cmd` の中に閉じるだけで足りる。
- `tools/archtest` に `webauthn-only-dep` 規則を追加した (`rules.go`)。`fxamacker/cbor/v2`・`x448/float16` を import してよいのは `cmd/internal/webauthn/**` だけ、という、既存の `os/exec` 制限と同じ形の機械的な検査。

### ライブラリの選定 (確定)

2026-09-28 時点で調査した候補:

| 候補 | 直接依存 | ライセンス | 特徴 |
|---|---|---|---|
| A. `fxamacker/cbor/v2` (CBOR だけ) + 自前のセレモニー実装 | `x448/float16` の 1 つ (どちらも MIT) | MIT | 依存の footprint が最小。セレモニーの正しさは自分の責任。 |
| B. `go-webauthn/webauthn` (RP ライブラリ一式) | 直接だけで 9 個 (`go-tpm`・`golang-jwt` 等。2026-09-28 に `go.mod` で確認) | BSD-3 | 実績があるが pre-1.0、goronation に不要な機能 (TPM 等の企業向け attestation・JWT) まで付く。 |

**候補 A に決めた**: goronation serve は、1 ユーザー・1 passkey・企業向けの attestation ポリシーを要求しない最小限の骨組みで、候補 B の機能の大半を使わない。ES256 (P-256 ECDSA) だけに絞り、attestation は "none" だけを受理する (信頼チェーンは検証しない) ことで実装量を抑え、セレモニーの正しさは否定的なテスト (偽の origin・challenge・署名・rpIdHash・別鍵の署名など。`ceremony_test.go`) で担保する。

## 帰結

- `cmd` module の `go.sum` に、初めて外部依存 (`fxamacker/cbor/v2`・`x448/float16`) が載る。他の module は引き続き依存ゼロ。
- `tools/archtest` の `webauthn-only-dep` が、依存の広がりを CI (`make check`) で機械的に検査する。
- 依存の更新 (CVE 対応・破壊的変更への追随) という保守作業が、`cmd/internal/webauthn` にだけ生じる。`govulncheck` 等を CI に足すかは、この ADR では決めない。
- 依存ゼロの方針そのものは、この 1 点以外では変えない。新しく依存を足したくなったら、この ADR とは別に、その都度、判断と記録を残す。

## 代替案

検討して却下した案:

- **CBOR も自前実装する**: 認証バイパスに直結しうるパースコードを、fuzz 済みの既存実装より自分で書く方が安全、とは判断しなかった。
- **WebAuthn 自体を諦め、別の認証方式にする**: Issue #1 で「WebAuthn 必須」とすでに決まっており、この ADR の対象外。
- **依存ゼロの方針を、リポジトリ全体で撤回する**: 必要は WebAuthn の検証コードに限られ、他のコード (檻・egress・hostfs など) まで依存を許す理由は無い。
- **新しい top-level module にする**: `cmd/goronation` が静的にリンクする以上、`cmd` module の `go.sum` が cbor を持つことは module を分けても避けられない (帰結を参照)。守りたいもの (他の 4 module の依存ゼロ) は `cmd` の中に閉じるだけで足り、module を分ける手間には見合わないと判断した。候補 B を選んでいれば、依存の多さからこの判断が変わった可能性がある。

# 0032. Vault の port は core に置き、資格情報の取り出しは既存の `credential.Source` を使う (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (構成図・未決)、ADR 0031・0036、`core/credential`

## 状況

Issue の構成図は、core の port に `vault.Signer` を挙げるが、コードには無い。実際の core には、資格情報の取り出し口 `credential.Source` (`Token(ctx, name) (Secret, error)`) が既にあり、doc が「実装は、ホストのファイル・将来の Vault」と書く。`egress/gateway` は、要求ごとに `Token` を呼ぶ。ログイン状態の保管・復元の port は、無い。

## 決定

1. **注入用の lookup は、新しい port を作らず、`credential.Source` をそのまま使う。** Vault は、その実装の 1 つになる。名前の形の検査 (`CheckName`)・`Secret` の隠し方・`ErrNotFound` は、変えない。egress は、Vault を知らない。
2. **`core/vault` を新設し、次の 2 つの port を置く。** package 名は、Issue の `vault.Signer` に合わせる (`core/vault` は port だけで、実装は top module の `vault/`)。
   - `Signer`: 署名の要求 (鍵の識別子と署名対象) を受け、署名を返す。鍵は返さない。要求ごとに、監査の記録を残す。
   - `LoginStore`: エージェント名 (`credential.CheckName` の形) を鍵に、ログイン状態を `Put`・`Get` する。状態は、名前つきのファイルの一覧 (内容はバイト列)。無ければ `ErrNotFound`。**内容は、檻の中のエージェントが書いた認証ファイルで、敵対入力として扱う。** 上限: 1 状態あたり 8 ファイル・1 ファイル 64 KiB・合計 256 KiB。名前は、相対 (`filepath.IsLocal`)・128 バイト以内・重複なし。`Put` でも `Get` (復元) でも、同じ検査を通す。超える・違反は、保管せず、エラー (利用者に見える形で返す)。
3. **どの port も、Vault が施錠中なら、`vault.ErrLocked` を返す** (`errors.Is` で判定する。`core/vault` に置く)。`credential.Source` の契約にも、施錠中の `ErrLocked` を足す (実装の PR で doc を直す)。呼び手は、失敗として扱い、値の無い状態で続けない。
4. **解錠・passkey の追加・削除・reset は、port にしない。** これらは、Vault の実装 (`vault/`) の操作で、呼べるのは `cmd/**` (Vault のデーモン。ADR 0037) だけ (ADR 0036)。core の port を通して、他の module が解錠できてはならない。egress 側の `credential.Source` の実装は、デーモンへの UDS のクライアントで、`vault/` を import しない。

## 帰結

- egress・agent・sandbox は、`core` の port だけを見る。Vault の差し替え (テスト用の偽物・将来の別の実装) が、呼び手に及ばない。
- ログイン状態を「ファイルの一覧」にしたので、claude (`.credentials.json`) と opencode (`auth.json`・`mcp-auth.json`) の両方を、同じ形で持てる。エージェントごとの復元の細部は、`cmd/goronation` の profile が持つ。
- `vault.Signer` は、gpg を動かす形 (ADR 0031 決定 4) が決まるまで、interface だけを置く。

## 代替案

- 注入用の lookup を、新しい `vault.Lookup` として足す: `credential.Source` と重複し、egress の呼び手が 2 種類の口を持つ。
- ログイン状態を、`credential.Source` の名前の 1 つとして持つ: 複数のファイルと、「同じエージェントにだけ復元する」制約を、名前と `Secret` (文字列 1 つ) で表せない。

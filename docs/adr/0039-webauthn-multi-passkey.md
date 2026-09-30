# 0039. webauthn は、複数の passkey と、操作に束縛した再認証を持ち、ED は許可リストで受ける (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: ADR 0004・0033・0035・0037・0038、`cmd/internal/webauthn`

## 状況

ADR 0037 決定 4 は、解錠・追加・削除の challenge の作成と assertion の検証を `web` (`cmd/internal/webauthn`) に置く。これまでは、1 passkey だけで、ED (拡張データ) を拒否し、PRF の結果を読まなかった。実機の PRF・ED の挙動は、S9 で確かめる。

## 決定

1. **passkey は最大 16 個** (Vault のラップの上限と同じ)。保存は `credentials` の配列で、単数の版の `credential` は、読むときに移す。`credfile` の値の上限 (1 KiB) に収まらないので、`credfile.NewSized` で、この Store だけ 16 KiB にする (値の形と安全の検査は同じ)。credential ID は 128 バイトまで。
2. **ED フラグを受ける。** 拡張データは CBOR の map 1 つで、末尾まで余りなし・4 項目まで・キーは許可リスト (`hmac-secret`・`credProtect`)・型を検査する。許可外は拒否する (fail closed)。実機でキーが足りなければ、S9 の結果で足す。
3. **PRF は `clientExtensionResults` から読み、検証しない値として渡す** (ADR 0033 決定 5)。`Assertion.PRF`・`Candidate.PRF` は、呼び手が復号を試す入力にだけ使い、`Wipe` で消す。
4. **操作つきの再認証 (`OpAuthBegin`・`OpAuthFinish`)。** UV 必須。state token が、操作 (解錠・追加・削除)・対象の credential ID・要求の ID・応えてよい passkey を署名の中に持つ。Finish は、呼び手の束縛との一致を見る。応答は、プロセスの中で 1 回しか通らない。認証 (ログイン) の state とは、目的が違うので、互いに使えない。
5. **追加 (`AddBegin`→`AddFinish`→`CommitAdd`)。** 登録 (UV 必須・既存は excludeCredentials・新しい PRF の salt を渡す) で候補を得て、保存はしない。既存の passkey が、同じ要求・対象への OpAddCredential の認証を通したとき (`Assertion`) だけ、`CommitAdd` が保存する。候補自身は、認可に使えない。ブートストラップトークンが要るのは、最初の 1 つだけ。
6. **削除 (`RemoveCredential`)。** 消す passkey 以外の認可・最後の 1 つは消せない。発行済みのセッションは、全て失効させる。Vault のラップの削除 (`vault.RemoveWrap`) は、呼び手が別に行う。
7. **Vault と web のログインの順序は呼び手が決める。** ADR 0038 決定 3 のとおり、候補は、web のログインに登録済みで Vault に無いものだけなので、`CommitAdd` の後に、`vault.AddWrap` を呼ぶ。

## 帰結

- 作成時に PRF が返らない passkey は、`Candidate.PRF` が nil になる。ラップを作るには、追加した passkey での認証が、別に要る (後続の PR。S9 で、作成時に返るかを確かめる)。
- SAS (ADR 0035) の表示・要求の ID の管理・UDS は、この package の外。束縛の検査だけが、ここにある。
- 単数の版に戻すと、状態は、未登録に見える (ブートストラップで登録し直す)。

## 代替案

- passkey ごとに別のファイルを持つ: 追加・削除が、複数ファイルの原子性を失う。
- `credfile` の既定の上限を上げる: 他の資格情報 (トークン) の上限も変わる。

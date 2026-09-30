# 0033. Vault の鍵は、passkey の PRF から作るラップ鍵で、本体鍵を包む (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (「Vault (Passkey PRF)」)、ADR 0031・0034・0035・0037・0038

## 状況

Vault は、passkey の WebAuthn PRF 拡張の出力で封じる。passkey は複数登録でき、1 つでも運用できる。ccserver (別リポジトリ) は、同じ設計で、クライアント申告の PRF 値によるラップの置き換え・ロックアウトなどの不具合を、監査で見つけて直した。

## 決定

1. **3 段の鍵にする。** PRF 出力 (32 バイト) → **ラップ鍵** (passkey ごと) → **本体鍵** (Vault に 1 つ・`crypto/rand` の 32 バイト)。本体鍵は、デーモンのメモリ (ADR 0037) の外に、平文で出さない。
2. **ラップ鍵は、HKDF-SHA256 で作る。** 入力は PRF 出力、salt は Vault の ID (16 バイトの乱数)、info は用途の固定の文字列と credential ID。本体鍵を AEAD (AES-256-GCM) で包み、passkey ごとに保存する (AAD は Vault の ID と credential ID)。
3. **PRF に渡す salt は、passkey ごとの 32 バイトの乱数で、ラップと一緒に保存する** (秘密ではない)。デーモンが作り、クライアントからは受けない。認証では、各 passkey に、自分の salt を渡す (`evalByCredential`)。
4. **項目は、書き込みごとに鍵を変える。** 書き込みごとに 16 バイトの乱数を作り、本体鍵から HKDF (info は種類・名前・その乱数) で項目の鍵を導いて、AEAD 暗号化する。鍵が毎回変わるので、nonce (12 バイトの固定) の再利用の議論は要らない。AAD は Vault の ID・種類 (資格情報・署名鍵・ログイン状態)・名前・形式の版。形式の版が合わない Vault は、解錠しない (削除と reset だけ)。
5. **クライアントが申告した PRF 値は、復号を試す入力にだけ使う。** PRF の結果は、`clientExtensionResults` にあり、署名の対象 (authenticatorData と clientDataHash) の外なので、クライアントが任意の値を送れる。既存の passkey では、誤った値は復号の失敗になるだけで、無害。未登録の passkey の値は、検証できない (ADR 0038)。失敗しても、保存内容は変わらない。偽の PRF 値でラップが変わらないことを、回帰テストで固定する。
6. **salt は、回さない。** 次の salt の PRF 出力は、クライアント申告で検証できない。儀式を 1 回通せる者が、任意の値でラップを作り替え、持ち主を恒久的に締め出せる (ccserver は、これで廃止した)。PRF 出力は、ログにも監査の記録にも残さず、使い終えたら `clear` する。
7. **一度漏れた PRF 出力は、自動では失効しない。** 対処は reset (ADR 0034)。
8. **暗号は、標準ライブラリだけ** (`crypto/hkdf`・`crypto/aes`・`crypto/cipher`・`crypto/rand`)。
9. **ラップの集合を変えるのは、ADR 0038 の 2 か所だけ。**

## 帰結

- passkey の追加・削除で、本体鍵と保管した内容は、作り直さない。
- ラップの ID・salt・暗号文は、ディスクに残る。PRF 出力が無ければ復号できない。
- 解錠の有効期間・施錠の契機 (無操作の時間など) は、別の決定にする。

## 代替案

- 項目の鍵を、PRF から直接作る: 追加のたびに、全項目を暗号化し直す。
- salt を、Vault の ID から作る固定値にする: passkey 間で salt が共有され、`evalByCredential` と合わない。
- salt を回して失効する: 検証できない値で、ラップを作り替えられる (決定 6)。

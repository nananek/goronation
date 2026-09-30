# 0037. Vault の本体鍵は、専用のデーモンだけが持ち、他のプロセスは UDS で引く (M2)

- 状態: 採用
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0012・0031・0032・0033・0038、`cmd/goronation/run.go` (`newPushConfig`)

## 状況

資格情報を使う egress は、`goronation run` (CLI) と `goronation serve` の別のプロセスで動く (`run` は `credfile.New(stateDir)` を自分で作る)。解錠の儀式は `goronation web` にあり、`run` には解錠の手段が無い。本体鍵を持つプロセスと、値を引く経路は、未決だった。

## 決定

1. **本体鍵は、専用のデーモン `goronation vault serve` のメモリだけに置く。** ネットワークの入力を解析する `web` には、置かない。デーモンは、operator (systemd の user unit かシェル) が起動する長寿命のプロセスで、他の再起動をまたいで解錠を保つ。root では起動しない (ADR 0012)。
2. **他のプロセスは、UDS (`<state>/vault/vault.sock`) で使う。** 親のディレクトリは 0700・socket は 0600。接続ごとに `SO_PEERCRED` を見て、uid がデーモンと違えば拒否し、記録する。檻には、この path を見せない (abstract socket は使わない)。
3. **API は 2 種類だけ。** 参照 (`Token`・`Sign`・`LoginStore.Get`) と、操作 (`Unlock`・`Lock`・`AddWrap`・`RemoveWrap`・`Put`・`Status`)。値の一覧・一括・本体鍵の取り出しは、作らない。名前は `credential.CheckName` の形だけ。施錠中の参照は `ErrLocked`。
4. **解錠・追加・削除の challenge の作成と、assertion の検証 (操作の種類・対象の credential ID・要求の ID への束縛) は、`web` が行う** (`cmd/internal/webauthn`)。デーモンは、固定形式の要求と PRF 出力のバイト列だけを受け、**ラップの復号ができるか** (暗号的な証明。ADR 0038) だけを見る。WebAuthn の解析 (CBOR・clientDataJSON・署名) と、外部の CBOR の依存は、鍵を持つデーモンに入れない。`web` は、PRF 出力を、渡した直後に消す。
5. **参照は、要求ごとに記録する** (時刻・操作・名前・相手の pid と実行ファイル・結果。値は書かない)。
6. **`run`・`serve` の egress は、UDS のクライアント (`cmd/internal` に置く `credential.Source` の実装) で引く。** デーモンが動いていない・施錠中なら、失敗し、次の行動 (動いていなければ `goronation vault serve` を起動する・施錠中なら web で解錠する) を、エラーで言う。`credfile` に、fall back しない。
7. **移行。** Vault を有効にする (`<state>/vault/` を作る) までは、`credfile` のまま (ADR 0031 決定 5)。有効にした後、`goronation auth` は Vault に入れ、同じ名前の `credfile` を消す。Vault にある名前は、`credfile` から読まない。
8. **デーモンの守り。** 起動時に、`dumpable=0` (`setNonDumpable`)・`RLIMIT_CORE=0`・本体鍵の領域の `mlock` (可能なら。失敗は警告して続ける)。鍵は 1 つの `[]byte` に置き、コピーせず、施錠で `clear` する。同じ uid の `ptrace`・`/proc/<pid>/mem` を、kernel が断る。

## 帰結

- **同じ uid の任意のプロセスは、解錠中に、参照 API で値を取れる** (`SO_PEERCRED` は uid だけを見る)。いまの `credfile` (0600・平文) と同じ信頼の範囲で、受け入れる。緩和は、施錠・要求ごとの記録・一括の取り出しが無いこと。別の uid で動かすのは、運用が重い。
- `run` は、単体では解錠できない。事前に web で解錠する。デーモンの障害は、全ての注入を止める (fail closed)。
- **脅威は 2 つに分ける。M1 (cookie の乗っ取り・`web` は正常)**: SAS と操作の束縛 (ADR 0035・0038) で守る。**M2 (`web` のプロセスの乗っ取り)**: PRF 出力が `web` を通るので、守れない。乗っ取られた `web` は、利用者に承認させた解錠の PRF を奪い、`<state>/vault/` のラップ (同じ uid に読める) と組み合わせて、以後、施錠と無関係に、オフラインで復号できる (対処は reset)。表示も `web` のものなので、操作の偽装もできる。本体鍵を `web` に置かない効果は、「`web` のメモリの読み取りだけでは、本体鍵が得られない」ことに限られる。

## 代替案

- 本体鍵を `web` に置く: 入力を解析するプロセスが鍵を持ち、`web` の再起動で施錠される。
- 解錠の結果を、ファイルで渡す: 鍵が、ディスクに出る。
- ブラウザが PRF を、デーモンへ直接送る (解錠のページを、`web` でなく、鍵を持つ側の別 origin から出す): デーモンが HTTP を待ち受ける。**M2 では、PRF が `web` を通らないこの構造でないと PRF を守れない。引き換えとして、今は採らず、後で判断し直せるように残す。**

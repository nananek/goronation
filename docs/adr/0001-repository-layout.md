# 0001. リポジトリ構成 (multi-module workspace と import 制約テスト)

- 状態: 採用
- 日付: 2026-09-25
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1) (M0「リポジトリ土台」)

## 状況

Issue #1 は次を求めている。

- 徹底したモジュール化。サンドボックスのバックエンドとエージェントのアダプタを、それぞれ独立してテスト・保守できること。
- macOS (seatbelt) を、別の保守者が担当できる独立モジュールにすること。
- `core` が backend / adapter の実装を import しないこと。
- 不変条件 I1 を機械的に強制すること。`os/exec` を import してよいパッケージを `sandbox/*` と `cmd/*` に限る。
- 単一バイナリで配布できること。

これらを、Go の module 構成と CI のテストでどう実現するかを決める必要がある。

## 決定

### 1. module の粒度は「トップディレクトリ = 1 module」

`core` `sandbox` `cmd` など、トップディレクトリごとに 1 つの module (`go.mod`) とし、`go.work` で束ねる。`sandbox/bwrap` のような下位ディレクトリは module ではなくパッケージとする。module path は `github.com/nananek/goronation/<相対dir>` に固定する。

macOS 保守者の分離は、CODEOWNERS と import 規則で足りる。seatbelt を別 module にするかどうかは、M4 で再検討する。

### 2. workspace のルートでは `go build ./...` を使わず、Makefile が全 module を列挙する

workspace のルートで `go build ./...` を実行すると `pattern ./...: directory prefix . does not contain modules listed in go.work` で失敗する (実測)。Makefile は `go list -m -f '{{.Dir}}'` で全 module を列挙し、各 module で実行する。

### 3. 「core は実装を import しない」の強制は archtest が担う

workspace モードでは、`go.mod` に `require` が無くても、他の module のパッケージを import できてしまう (実測: `core` が `sandbox/bwrap` を import しても `go build` は通る。`GOWORK=off` では落ちる)。

そのため、module 分割は依存方向の強制にならない。**`go.mod` では依存方向を守れず、archtest が唯一の防壁になる。** これが、次の archtest を置く直接の理由である。

### 4. import 制約は `tools/archtest` に置く

Issue #1 の構成図に無い `tools/archtest` module を追加する。検査は全 module を見渡せる場所に置く必要があり、いずれかの module の中に置くと、その module の依存関係に検査対象が縛られるため。

- 標準ライブラリだけで書く。`go/parser` と `go/ast` で構文解析し、`go list` も `os/exec` も使わない。archtest 自身も規則の適用対象で、自己免除しない。
- ソースを **全ビルドタグ・全 `_test.go` を含めて** 解析する。`//go:build darwin` の側に隠れた違反も拾うため。ビルド済みパッケージ一覧 (`go/packages`) は使わない。
- symlink の `.go` は go tool が通常のファイルとして build する (実測: `go list` の `GoFiles` に入る) ため、link の位置のファイルとして検査する。追わないと、許可された場所 (`sandbox/**`) のファイルへの symlink を `core/` に置くだけで規則をすり抜けられる。symlink の `go.mod` も go tool はそのまま読む (実測: 指す先の `module` が workspace の module path になる) ため、同じく link の位置で検査する (`modpath`)。symlink のディレクトリは、辿らずに **error** にする (決定 7)。
- `.git` / `testdata` / `vendor` と、`.` または `_` で始まるディレクトリは走査しない (go tool の `./...` の列挙と同じ)。その代わり、リポジトリ内の import path がこれらの名前のセグメントを含んだら、規則 `unscanned-dir-import` の違反にする。許可される場所は無い (決定 7)。
- 規則は許可リスト方式で、`tools/archtest/rules.go` の表に置く。規則の追加や緩和は表の変更だけで済む。
- **fail-closed**。root が見つからない、または走査した `.go` が 0 ファイルなら、テストは失敗する (空振りで緑にしない)。
- `go.mod` の `module` 行が規約と一致することも検査する (`modpath`)。ずれると import 規則が黙って空振りするため。

規則の一覧 (`exec-import` / `exec-call` / `cgo` / `dep-core` / `dep-agent-sandbox` / `impl-only-from-cmd` / `modpath` / `unscanned-dir-import`) のうち、表で持つものは `tools/archtest/rules.go` を正とする。`modpath` と `unscanned-dir-import` は、規則表ではなく検査器 (`tools/archtest/archtest.go`) に組み込んでいる。

### 5. CI は同じ `make check` を 2 つの leg で回し、環境変数だけを変える

- `base` leg: bwrap が **無い** ことを保証して実行する。bwrap を要するテストは skip される。
- `bwrap` leg: `GORO_REQUIRE_BWRAP=1` で実行する。bwrap が使えなければ skip ではなく **fail** する。実行層の leg が、黙って全 skip で緑になるのを防ぐ。

job 名 `base` / `bwrap` はブランチ保護の required check 名として固定する。どちらの leg も `GO_TEST_EXTRA=-v` を渡し、skip の理由と bwrap の版をログに残す。

Actions (`actions/checkout`、`actions/setup-go`) は commit SHA でピン留めする (信頼できないものを扱う基盤の方針に合わせる)。`ci.yml` の SHA の末尾のコメントが tag で、更新するときは tag から SHA を引き直して両方を書き換える。

### 6. bwrap は固定パス `/usr/bin/bwrap` で呼ぶ

PATH を検索しない (設計ルール 1 の「固定パスの起動器」に合わせる)。引数はコード内の定数だけで作る。

### 7. 走査しないディレクトリと、ディレクトリの symlink を、迂回の抜け道にしない (plan 6.1 からの変更)

plan 6.1 は、走査から `testdata` / `vendor` / `.` または `_` で始まるディレクトリを除き、symlink は追わない (go tool と同じ) と定めていた。これを次のとおり変更する。

- リポジトリ内の import path が、走査しないディレクトリの名前のセグメントを含んだら違反にする (規則 `unscanned-dir-import`)。
- 走査対象のツリー内のディレクトリの symlink は、辿らずに error にする (fail-closed)。走査しないディレクトリの名前の symlink は、実体のディレクトリと同じく走査せず、その import を上の規則が違反にする。

理由: go tool の `./...` は、除外ディレクトリも symlink のディレクトリも列挙しない。しかし、明示的に import されれば、そこにある package を build する。plan のまま黙って無視すると、それらが規則の抜け道になる。実測 (`core` から import すると、`go build` が通り、`go list -deps` で `os/exec` に依存し、変更前の archtest は緑のままだった): `core/_hidden`、`core/.hidden`、`core/testdata`、`core/vendor`、`sandbox/` 配下へのディレクトリ symlink (`core/runner`)。変更後は、5 つとも archtest が赤になる。symlink の `.go` を link の位置で検査する (決定 4) のと同じ考え方である。

代償: これらの名前のディレクトリに置いたコードを、リポジトリ内の他のコードから import できなくなる。ツリー内にディレクトリの symlink を置けなくなる (走査しないディレクトリの中を除く)。どちらも、いまのリポジトリに該当するものは無い。

## 帰結

- 依存方向と I1 が、CI のテストで機械的に守られる。違反はビルドを落とす。
- `go.mod` では依存方向が守れないため、archtest の規則表が唯一の防壁になる。規則表の変更は、レビューで特に注意して見る。
- **既知の限界**: 静的検査なので、`//go:linkname`、`reflect`、生の `syscall.Syscall(SYS_EXECVE, ...)` などで回避できる。強制の主体は設計ルール 1 とレビューであり、このテストは **事故防止** である。悪意ある実装への防壁ではない。
- **既知の限界 (`impl-only-from-cmd`)**: 規則は plan の文言どおり「実装 package (`sandbox/bwrap/**` など) を import してよいのは `cmd/**` だけ」と置いている。そのため、実装 package 自身の側の import も違反になる (実測): 自 package 配下の `sandbox/bwrap/internal/*` の import、外部テストパッケージ (`package bwrap_test`) から `sandbox/bwrap` の import、`sandbox/conformance` から `sandbox/bwrap` の import。M0 の時点では該当するコードが無く、緑である。**M1 で実装 package が内部構造やテストを持った時点で、`tools/archtest/rules.go` の表の変更が要る** (自 package 配下と `sandbox/conformance` を許可に足す、など)。誤検出の側 (fail-closed) に倒れているので、いまは表を直さない。
- **既知の限界 (規則表に無い起動 API)**: プロセス起動として検出するのは、`os/exec` の import と、`exec-call` の表にある関数 (`os.StartProcess` など) だけである。表に無い API は検出しない。実測: `net/http/cgi` は内部で `os/exec` を使って子プロセスを起動する (標準ライブラリの中の import なので `exec-import` の対象外) が、`core` から使っても、`go build` が通って `os/exec` に依存し、archtest は緑のままだった。`net/http/cgi` など、標準ライブラリの内部で子プロセスを起動する API は、未対応である。表に足せる (`rules.go`) が、網羅はできない。
- **既知の限界 (`go.work` の `use` による、走査しないディレクトリの module)**: 決定 7 は、リポジトリ内の import path を見ている。`go.work` の `use` で走査しないディレクトリ (例: `core/_evil`) を module として取り込み、その `go.mod` に別の module path (例: `example.com/evil`) を付けると、その path はリポジトリ内の import path ではなく、中のコードも走査されない。実測: `core/_evil/evil.go` が `os/exec` を使い、`core` が `example.com/evil` を import しても、`go build` が通って `os/exec` に依存し、archtest は緑のままだった。`go.work` の変更として差分に現れるため、レビューで見る。閉じるなら、`go.work` の `use` を読み、走査しないディレクトリを指すものを違反にする、などが要る。
- `-race` は cgo を要するため、ローカルに gcc が無いと `make test` が落ちる (Makefile のコメントに逃げ道を書く)。最終成果物は `CGO_ENABLED=0` でビルドする。
- `sandbox/**` の許可範囲は Issue の文言どおりで広く、`sandbox/conformance` も含む。M1 で `bwrap` / `seatbelt` / `init` のみに絞れるか再検討する。
- I1 の解釈は Issue #1 で未決である (入力が全てデーモン由来のものに限り、外部バイナリのホスト直実行を許すか)。この ADR は現行文言どおりの **厳格解釈** で規則を置く。解釈が緩和された場合は、ADR を書いて規則表を直す。
- GitHub の ubuntu-24.04 runner で、bwrap が unprivileged user namespace の制限 (AppArmor の `kernel.apparmor_restrict_unprivileged_userns`) に阻まれる可能性がある。**未検証** で、初回の CI で確定する。結果は、この ADR への追記か別 ADR に残す。

## 代替案

### 単一 module + `internal/` 制約 (却下)

1 つの module にして、Go の `internal/` ディレクトリの規則で依存を制限する案。

- `internal/` は「親ディレクトリ配下からしか import できない」という粗い制限で、「`agent` と `sandbox` は互いに import しない」「実装を import してよいのは `cmd` だけ」といった横方向の規則を表せない。
- macOS 保守者が、他の領域と同じ module 内で作業することになり、依存と CODEOWNERS の両面で分離が弱くなる。
- 「決定 3」のとおり、multi-module にしても go.mod では依存を守れないが、単一 module では最初から守る手段が `internal/` しかなく、その表現力が足りない。

### `go list` / `go/packages` を使う検査 (却下)

ビルド済みのパッケージ一覧から import を調べる案。現在のビルドタグで選ばれたファイルしか見えず、`//go:build darwin` 側に隠れた違反を拾えない。また `go list` を呼ぶには `os/exec` が必要で、archtest 自身が規則の対象外になってしまう。

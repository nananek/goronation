# goronation

goronation は、**信頼できない AI コーディングエージェントを檻 (サンドボックス) に閉じ込めたまま**、手元の端末や遠隔 (Tailscale 越しのスマートフォンなど) から安全に操作するための基盤です。対応エージェントは Claude Code と OpenCode のみ、実装言語は Go (単一バイナリで配布)、対応 OS は Linux (bwrap) が先行で、macOS (seatbelt) は後続の独立モジュールです。前身の ccserver (Node.js) からは仕様・テスト・教訓だけを持ち込み、コードは持ち込みません。

ロードマップ・不変条件・設計ルールの正本は [Issue #1](https://github.com/nananek/goronation/issues/1) です。決まったことはそこで更新します。

## 現在の状態

M0 (土台と spike) の途中です。リポジトリ土台 (multi-module workspace、CI の leg、import 制約テスト、CODEOWNERS、ADR の置き場) だけがあり、機能はまだありません。

## 構成

Go の multi-module workspace で、トップディレクトリ 1 つが 1 module です。

| ディレクトリ | 内容 |
|---|---|
| `core/` | domain 型と ports。backend / adapter の実装を import しない |
| `sandbox/` | サンドボックスのバックエンド (`sandbox/bwrap` など) |
| `cmd/` | 実行ファイル (`cmd/goro`)。実装の配線はここだけで行う |
| `tools/archtest/` | import 制約テスト (依存方向と不変条件 I1 を機械的に強制する) |
| `docs/adr/` | ADR (設計判断の記録) |

`spec/`・`control/`・`agent/`・`egress/`・`vault/`・`hostfs/`・`gateway/`・`web/` は、最初に必要になった時点で作ります。全体像は Issue #1 の構成図を参照してください。

## 開発コマンド

必要なもの: Go 1.24 以降。`-race` のため cgo (gcc) も必要です (ローカルに gcc が無い場合は Makefile のコメントを参照)。

| コマンド | 内容 |
|---|---|
| `make check` | 下記をすべて実行する (CI が回すのはこれ) |
| `make fmt-check` | `gofmt -l` の出力が空であること |
| `make vet` | 全 module に `go vet ./...` |
| `make test` | 全 module に `go test -race -count=1 ./...` (追加のフラグは `GO_TEST_EXTRA` で渡す) |
| `make build` | `CGO_ENABLED=0` で `bin/goro` を作る (単一バイナリと cgo 禁止の裏取り) |
| `make test-bwrap` | `GORO_REQUIRE_BWRAP=1` で `make test` を実行する |

workspace のルートでは `go build ./...` が使えません (`directory prefix . does not contain modules listed in go.work`)。Makefile が `go list -m` で全 module を列挙し、各 module で実行します。

## CI の leg

CI は同じ `make check` を 2 つの leg で回し、環境だけを変えます。どちらも `GO_TEST_EXTRA=-v` を渡し、skip の理由と bwrap の版をログに出します (`go test` は `-v` が無いと出力しません)。job 名 (`base` / `bwrap`) はブランチ保護の required check 名として固定です。

| leg | 環境 | 意味 |
|---|---|---|
| `base` | bwrap が **無い** ことを確認して実行 | 基盤なしで通る層。bwrap を要するテストは skip される (理由が出る) |
| `bwrap` | bubblewrap を入れ、`GORO_REQUIRE_BWRAP=1` で実行 | 実行層。bwrap が使えなければ skip ではなく **fail** する |

### `GORO_REQUIRE_BWRAP`

`1` のとき、bwrap を要するテストは、`/usr/bin/bwrap` が無い・起動できない場合に skip せず失敗します。実行層の leg が、全部 skip されたまま緑になるのを防ぐためです。未設定のときは skip します。

## import 制約テスト

`tools/archtest` は、Go のソースを構文解析して、次の規則を機械的に検査します (全ビルドタグ・全 `_test.go` が対象)。規則の表は `tools/archtest/rules.go` にあります。

- `os/exec` (と、プロセス起動の呼び出し) を使ってよいのは `sandbox/**` と `cmd/**` だけ (不変条件 I1)。
- `import "C"` は禁止 (単一バイナリ / `CGO_ENABLED=0`)。
- `core` は他の module を import しない。`agent` と `sandbox` は互いに import しない。
- bwrap / seatbelt / エージェントの実装を import してよいのは `cmd/**` だけ。
- 各 `go.mod` の module path が規約どおりであること。

**限界**: 静的検査なので、`//go:linkname`、`reflect`、生の `syscall.Syscall(SYS_EXECVE, ...)` などで回避できます。強制の主体は設計ルールとレビューであり、このテストは **事故防止** です。悪意ある実装への防壁ではありません。詳細は [ADR 0001](docs/adr/0001-repository-layout.md) を参照してください。

## ADR

設計判断と spike の結果は [`docs/adr/`](docs/adr/README.md) に残します。書き方とテンプレートは [`docs/adr/README.md`](docs/adr/README.md) を参照してください。

## ライセンス

**未確定です** (MIT を検討中で、依存ライブラリのライセンス次第で決めます)。確定するまで `LICENSE` は置きません。外部依存も追加していません。

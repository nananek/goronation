# 0026. 耐久イベントログに限り、`modernc.org/sqlite` を使う (依存ゼロの方針の例外)

- 状態: 採用 (2026-10-01 に追記: `sqlite-only-dep` 規則 (`modernc.org/sqlite/**`・`modernc.org/libc/**`) と、`govulncheck` の CI job (`make vulncheck`。push・PR・週次。required check にしない) を実装した)
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、[ADR 0004](0004-webauthn-dependency-exception.md)・[ADR 0005](0005-websocket-dependency-exception.md) (同じ枠組みの先例)、ADR 0023

## 状況

- ADR 0023 のログを、自前のセグメントファイルでなく SQLite に置く。CGO 禁止 (`CGO_ENABLED=0` の単一バイナリ) と両立するのは、pure Go の `modernc.org/sqlite` である。
- 実測 (2026-09-30): 最新の v1.60.1 は go 1.26.0 を要求する。go.work は go 1.24.0 で、CI は go.work の版を使う。**v1.40.0 は go 1.24.0 で足りる。** v1.40.0 で `CGO_ENABLED=0 go build` が通り、ハローワールド相当で約 9 MB (約 +7 MB)。推移的な依存は約 10 module (`modernc.org/{libc,mathutil,memory}`・`golang.org/x/{exp,sys}`・`google/uuid`・`dustin/go-humanize`・`mattn/go-isatty`・`ncruces/go-strftime`・`remyoudompheng/bigfft`)。
- DB を 0600 で先に作ると、`-wal`・`-shm` も 0600 で作られる (umask 022 で実測)。

## 決定

- **耐久イベントログ (`cmd/internal/eventlog`) に限り**、`modernc.org/sqlite` を使ってよい、という依存ゼロの方針の例外を認める。
- **v1.40.0 に固定する。** go.work・CI の go の版を上げない。版を上げるのは、別の決定にする。
- 例外の範囲は、ADR 0023 のテーブルの読み書きだけ。SQL は固定の文で、値はバインド変数で渡す。
- `cmd` module の下の `cmd/internal/eventlog` に置く (ADR 0004・0005 と同じ理由)。他の module は依存ゼロのまま。
- `tools/archtest` に `sqlite-only-dep` 規則を足す (実装の PR)。`modernc.org/sqlite` を import してよいのは `cmd/internal/eventlog/**` だけ。

## 帰結

- `cmd` の `go.sum` に、上の約 10 module が載る。バイナリは約 7 MB 増える。
- 供給網の攻撃面が増える (C から機械変換された大きなコード)。入力は、goronation 自身が組んだ SQL と、検証した Event の JSON だけで、檻の入力を SQL として解釈しない。
- DB の破損・ロックの待ちは、縮退で受ける (ADR 0025)。
- **固定の間は、セキュリティ更新を、取り込めない期間ができる。** 追随の方針: 実装 PR (②) が `govulncheck` を CI に足し、push ごと・週次で `modernc.org/sqlite` の既知の脆弱性を確かめる。検出したら、版を上げる別の決定 (go の版の引き上げを含む) を、最優先で行う。
- **govulncheck の実測 (2026-10-01)**: `golang.org/x/vuln` v1.8.0 は go 1.26 を要し、go が toolchain を取りに行く (約 30 秒・ネットワークが要る)。このため、版 (v1.8.0) を固定して `go run` し、`make check` には入れない (新しい勧告が、無関係な変更を赤にする)。結果: コードが呼ぶ脆弱性は 0 件。要求している module に 1 件 (`golang.org/x/sys` の windows 専用 GO-2026-5024。この repo は windows を対象にしない)。

## 代替案

- **自前のセグメントファイル**: 依存ゼロだが、ローテーション・GC・上限・競合を自前で持つ (ADR 0023)。
- **`mattn/go-sqlite3`**: cgo が要り、`CGO_ENABLED=0` の単一バイナリと両立しない。
- **bbolt などの組み込み KV**: SQL の `SUM`・`DELETE ... WHERE` で足りるものを、自前で書くことになる。
- **最新の版 (v1.60.1)**: go 1.26 を要求し、go.work・CI の go の版を上げる必要がある。

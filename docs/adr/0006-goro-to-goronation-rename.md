# 0006. `goro` バイナリ名を `goronation` に統一する

- 状態: 採用
- 日付: 2026-09-28
- 関連: ADR 0001 (module path の決定)、`.claude/skills/reversibility-check`

## 状況

ADR 0001 で、module/repo の正式名は `github.com/nananek/goronation` (repo 名) に決めた。しかし CLI バイナリ名・`cmd/goro/` ディレクトリ名は、その決定とは別に、十分検討せずに短い "goro" にしてしまい、以後ずっと両者が食い違ったまま運用していた。

この種の不一致 (外から見える名前が、プロジェクトの正式名と違う) は、後から直すコストが、時間が経つほど上がる。実際、直す時点では、バイナリ名・`cmd/` のディレクトリ名・CLI の usage/エラーメッセージ・`GORO_` 環境変数 prefix・git ref の名前空間・`.claude/skills` の名前・内部マーカー文字列などに、91 ファイル・940 箇所ほど散らばっていた。

## 決定

外から見える "goro" の出現を、すべて "goronation" に置換する。対象は、バイナリ名・`cmd/goro` → `cmd/goronation` のディレクトリ・CLI の usage/エラーメッセージ・`GORO_PUSH_REPO`/`GORO_PUSH_REF_PREFIX`/`GORO_<AGENT名>` (利用者が直接使う公開系の環境変数)・`egress/git` の git ref 名前空間 (`refs/heads/goro/<セッション>/` → `refs/heads/goronation/<セッション>/`)・`sandbox/conformance`/`sandbox/bwrap` の内部マーカー文字列・`.claude/skills/goro-run`・`goro-add-agent` の名前。

`GORO_REQUIRE_BWRAP`・`GORO_CONFORMANCE_*`・`GORO_BWRAP_*`・`GORO_HOST_ONLY`・`GORO_TEST_MARKER` のような、テスト専用・内部の環境変数は対象外 (利用者に見せる公開 API ではないため、据え置く)。

## 帰結

- バイナリ名と module/repo 名が一致し、以後の新しい混乱を避けられる。
- 一方で、既定の state dir 名 (`~/.local/state/goro` → `~/.local/state/goronation`) が 6 バイト長くなった分、端末ビューの UDS socket path (AF_UNIX の 108 バイト上限。`README.md`・`goronation-run` skill に既に文書化されている既存の制約) に対する余裕が、その分だけ減る。実装中、この余裕の減りで、テストの UDS path が実際に 108 バイトを超えて失敗する例を確認した (`shortDir`・`tempSock` という、名前のとおり「短い path」を保証するための test helper が、自分の prefix にまで "goronation" を使ってしまっていたのが直接の原因。test helper 側の prefix を短くして直した。本番の `DefaultStateDir` 自体は、この rename の目的どおり `goronation` のままにしている)。HOME の path が長い利用者は、以前よりわずかに早くこの上限に当たりうるが、新しい種類のリスクではなく、既存の制約の余裕が減っただけであり、既存の対処 (`--state-dir` で短い path を指定する) がそのまま使える。

## 代替案

- **バイナリ名だけ、module 名 (`goronation`) とは別の短い名前 (例: `gn`) にする**: 不一致を無くすという、この rename の目的そのものを満たさない。却下。
- **`GORO_` 環境変数をテスト専用も含めて全部 `GORONATION_` にする**: 利用者に見えない内部の値まで変える理由が無く、変更範囲が徒に増えるだけ。却下 (依頼者と合意済み)。

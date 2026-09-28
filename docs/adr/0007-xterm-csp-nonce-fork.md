# 0007. xterm.js を CSP nonce 対応のためフォークし、upstream の新版を nvchecker で追跡する

- 状態: 採用 (2026-09-29 追記: パッチ対象の箇所数を 2 箇所から 3 箇所に訂正。下記参照)
- 日付: 2026-09-29
- 関連: Issue #46、upstream [xtermjs/xterm.js#4445](https://github.com/xtermjs/xterm.js/issues/4445)
  (CSP nonce 対応の feature request)、ADR 0004・0005 (依存の例外の先例)、
  `cmd/goronation/vendor/xterm/PATCH.md` (パッチの diff・ビルド/再適用手順の正本)

## 状況

- Issue #46: 端末ビューの CSP (`style-src 'self'`、`'unsafe-inline'` なし) の下で、xterm.js が
  実行時に動的生成する `<style>` 要素 (`DomRenderer` のテーマ色・セル寸法、`Viewport` のスクロール
  バーのスライダー配色) がブロックされる。CSP を緩める案は Issue #46 で既に却下済み。
- **上流に nonce 対応版は存在しない。** 上流 Issue #4445 (2023-03-23 open) が今も未解決。一度は
  別解 (Constructed StyleSheets、PR #4611) があったが revert され、vendor 済みの 6.0.0 でも
  `unsafe-inline` が要る。CSP を維持したまま解決するには、goronation 自身が xterm.js
  (MIT license) にパッチを当てるしかない。

## 決定

1. **xterm.js 6.0.0 (commit `f447274`) にローカルパッチを当てて vendor する。**
   `DomRenderer.ts`・`Viewport.ts` の `<style>` 生成箇所 3 つ (upstream がライブに動的生成する
   `<style>` の全て) に、新設した Terminal オプション `cspNonce` (既定 `null`) を `element.nonce`
   (IDL プロパティ) へ適用する処理を足す。未指定時は upstream と同一の挙動にし、既存 CSP のまま
   動作が変わらないことを担保する。自前ビルドした UMD バンドルを vendor し、**Node.js への依存は
   vendor 差し替え作業だけに限り、goronation 本体の Go ビルドには持ち込まない**。Go の `go.sum`
   には触れないため、ADR 0004・0005 と異なり依存ゼロ方針そのものの例外ではない。手順の詳細は
   `PATCH.md` に一元化する (ADR 0003)。
2. **upstream の新版を nvchecker + 週次 GitHub Actions
   (`.github/workflows/xterm-version-check.yml`) で追跡し、検知したら Issue を自動作成する。**
   Issue に「upstream #4445 が解決していないか確認し、解決していればこの fork/パッチを廃止して
   pristine な upstream ビルドに戻すことを検討する」チェック項目を含め、fork を恒久化させない。

## 帰結

- CSP を緩めずに Issue #46 を解決できる。
- xterm.js の vendor 更新のたびに、パッチの手動再適用が要る (コンフリクトの可能性は
  `PATCH.md` の手順で軽減するが、無くなりはしない)。
- CI に Python 製の nvchecker が増える。Go 側の依存には影響しないが、CI の構成要素は増える
  (外部 Action は `ci.yml` の慣行どおり commit SHA でピン留めした)。
- vendor している xterm.js が pristine な upstream ビルドではなくなり (差分は
  `csp-nonce.patch` で追跡)、upstream の以後の修正取り込みには都度コンフリクト解決が要る。

## 代替案

- **CSP を緩めて `'unsafe-inline'` を足す**: インラインスタイル注入対策の無効化になり、
  Issue #46 の議論で既に却下済み。
- **xterm.js の nonce 対応版へアップグレードする**: そのようなバージョンは存在せず、却下。
- **ビルド済み (minified) xterm.js への文字列パターン注入**: ツールチェーン不要で軽いが、
  バージョン更新のたびにミニファイ後のパターン再特定が要り脆い。ビルドツールチェーン整備の
  コストが見合うと判断し、TypeScript ソースへのパッチを採った。
- **nvchecker を使わず自前スクリプトで npm registry API を叩く**: 実装は小さいが、リトライ等を
  自前で持つことになる。実績あるツールの方が保守コストが低いと判断した。

## 訂正 (2026-09-29 追記)

当初のパッチ (初版) は `DomRenderer.ts` の2箇所だけが対象で、この ADR も「2箇所で全て」として
いた。独立レビューが実機ブラウザで、`Viewport.ts` のスクロールバーのスライダー配色にも同じ
パターンのライブな `<style>` 生成があり、そこだけ CSP でブロックされる (透明になる) ことを確認
した。「決定」節を訂正して `Viewport.ts` にもパッチを拡張済み (詳細は `PATCH.md`)。

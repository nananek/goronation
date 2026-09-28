# xterm.js への goronation ローカルパッチ (CSP nonce 対応)

## 背景

- Issue #46: 端末ビュー (`goro web`) の CSP (`style-src 'self'`、`'unsafe-inline'` なし) の下で、
  xterm.js の `DomRenderer` が実行時に動的生成する `<style>` 要素 (テーマ色・セル寸法) がブロックされる。
- 対応方針の検討 (詳細は `docs/adr/` ではなく、実装計画としてワーカー間で共有した
  `csp-nonce-plan` に記録済み): CSP を緩めず (`'unsafe-inline'` を足さず)、xterm.js 側に nonce
  対応を追加する。
- **上流に nonce 対応版は存在しない。** xterm.js 本体の CSP nonce 対応は上流 Issue
  [xtermjs/xterm.js#4445](https://github.com/xtermjs/xterm.js/issues/4445) (2023-03-23 open) として
  今も Open のまま。一度は別解 (Constructed StyleSheets で `unsafe-inline` 自体を不要にする PR #4611、
  5.3.0 でマージ) があったが 2023-08-15 に revert され、6.0.0 でも `unsafe-inline` が必要と upstream
  側で報告されている。
- このため、**goronation 自身が xterm.js (MIT license) にパッチを当てて nonce 対応を追加する**、
  という方針にした。

## vendor しているバージョンとパッチの範囲

- upstream: `@xterm/xterm` 6.0.0 (npm)、GitHub tag `6.0.0`
  (commit `f447274f430fd22513f6adbf9862d19524471c04`)。**この commit を正本の base とする**
  (`VERSION` ファイルに記録)。
- パッチ本体: [`csp-nonce.patch`](csp-nonce.patch) (`git diff` 形式、upstream リポジトリの
  6.0.0 タグからの差分)。対象は次の 4 ファイルのみ:
  - `src/browser/renderer/dom/DomRenderer.ts`: `<style>` を生成する2箇所
    (`_updateDimensions()` の `_dimensionsStyleElement`、`_injectCss()` の `_themeStyleElement`)
    それぞれの直後で、新設したプライベートメソッド `_setStyleElementNonce()` を呼び、
    `cspNonce` オプションが設定されていれば `element.nonce = <値>` を設定する。
  - `src/common/services/Services.ts` / `src/common/services/OptionsService.ts`:
    内部の `ITerminalOptions` / `DEFAULT_OPTIONS` に `cspNonce?: string | null` (既定値 `null`) を追加。
  - `typings/xterm.d.ts`: 公開 API の `ITerminalOptions` にも同じ `cspNonce` オプションを追加
    (goronation 側から `new Terminal({ ..., cspNonce })` として渡せるようにするための型定義)。
- **`element.nonce = value` というプロパティ経由の代入を使う。** `setAttribute('nonce', value)`
  ではない。HTML の仕様上、`nonce` はセキュリティ上の理由で一度設定されると属性としては反映されない
  (attribute reflection が意図的に隠される) ため、ページ読み込み後に動的生成した要素に対しては
  IDL プロパティ (`element.nonce = ...`) 経由でなければ確実に効かない。
- `cspNonce` が未設定 (既定値 `null`) のときの挙動は upstream と完全に同一
  (`nonce` 属性・プロパティを一切触らない)。**このパッチ単体では、既存の CSP
  (`style-src 'self'`、nonce 未使用) のままでも動作に変化がない**
  (`cmd/goronation/web_linux_test.go` の `TestWebTerminalPageHasNoInlineStyle` などの既存の回帰テストが
  そのまま通ることで担保する)。
- **サーバー側で実際に per-request nonce を生成し、`Terminal` 起動時に `cspNonce` として渡す配線は、
  このパッチ (PR①) の範囲外。** 別 PR (`terminal-page-csp-nonce`) で行う。

## ビルド手順 (再現・更新時)

Node.js は、この vendor 差し替え作業のときだけ使う。goronation 本体の Go ビルド
(`make build` / `cmd/goronation` の `go build`) は Node.js に一切依存しない。

```sh
# 1. upstream をパッチ対象の tag で取得する
git clone --branch <tag> https://github.com/xtermjs/xterm.js.git xterm-src
cd xterm-src

# 2. このパッチを当てる (バージョンが上がっている場合はコンフリクトすることがある。
#    その場合は csp-nonce.patch の内容を見ながら、DomRenderer.ts 等の該当箇所に手で当て直す)
git apply /path/to/goronation/cmd/goronation/vendor/xterm/csp-nonce.patch

# 3. 依存関係をインストールし、UMD バンドル (lib/xterm.js) をビルドする
npm ci
npm run package   # prepackage (tsc) → package (webpack, lib/xterm.js) → postpackage (esbuild, lib/xterm.mjs)

# 4. ビルド成果物を vendor に差し替える (goronation は UMD 版 (lib/xterm.js) だけを使う)
cp lib/xterm.js /path/to/goronation/cmd/goronation/vendor/xterm/xterm.js

# 5. VERSION ファイルと、このファイル (PATCH.md) のバージョン表記を更新する
# 6. go test ./... (cmd/goronation) を回し、既存の回帰テストが通ることを確認する
```

パッチが新しいバージョンにそのまま当たった場合、`csp-nonce.patch` 自体は差し替え不要
(diff の内容が同じであれば)。当たらなかった場合は、手で当て直した上で、この `csp-nonce.patch`
を新しい diff で上書きする。

## upstream が正式対応した場合の巻き戻し

upstream Issue #4445 が解決し、xterm.js が公式に CSP nonce (または同等の仕組み) に対応した場合、
このローカルパッチは不要になる。`cspNonce`, `_setStyleElementNonce` を検索して
(このリポジトリと、サーバー側の配線を行う PR の両方から) 置き換え、pristine な公式ビルドの
`xterm.js` に戻すことを検討する。`csp-nonce.patch` と、この `PATCH.md` も合わせて削除する。

このリポジトリでは、upstream の新バージョンが出るたびに `.github/workflows/xterm-version-check.yml`
(nvchecker) が Issue を自動作成し、その Issue のテンプレートに「upstream Issue #4445
(CSP nonce 対応) が入っていないか確認し、入っていればこの fork/パッチを廃止して upstream の素の
xterm.js に戻すことを検討する」という趣旨のチェック項目を含めている。つまりこの fork は恒久的な
前提ではなく、新バージョンが出るたびに「もう fork をやめられないか」を毎回検討する運用にしている。

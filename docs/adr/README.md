# ADR (Architecture Decision Records)

設計判断と spike の結果を、あとから根拠ごと辿れるように残す場所です。ドキュメントは日本語のみで書きます。

## 索引

| 番号 | タイトル | 状態 |
|---|---|---|
| [0000](0000-template.md) | テンプレート | - |
| [0001](0001-repository-layout.md) | リポジトリ構成 (multi-module workspace と import 制約テスト) | 採用 |
| [0002](0002-branch-workflow.md) | ブランチ運用 (develop を base とし、main は保守者が反映する) | 採用 |

## 書き方

1. [`0000-template.md`](0000-template.md) を `NNNN-<短い英語のスラッグ>.md` としてコピーする。番号は索引の最大値 + 1 で、欠番を作らない。
2. 状態・状況・決定・帰結・代替案を埋める。**却下した代替案とその理由**を必ず書く。
3. spike の ADR は、テンプレートの「spike の結果」の節も埋める。Issue #1 の「進め方」のとおり、**再現手順・fixtures の場所・go / no-go・成立しない場合の代替**を残して終わる。
4. この README の索引に 1 行足す (同じ PR で行う)。
5. 決定を変えるときは、既存の ADR を書き換えず、新しい ADR を書いて、古い ADR の状態を「置き換えられた (→ NNNN)」に変える。

## 状態

| 状態 | 意味 |
|---|---|
| 提案 | 議論中 |
| 採用 | 決定済みで、実装がこれに従う |
| 却下 | 採用しないと決めた (理由を残すため ADR は消さない) |
| 置き換えられた (→ NNNN) | 別の ADR に取って代わられた |

## 関連

- リポジトリの概要: [`../../README.md`](../../README.md)
- ロードマップと不変条件・設計ルール: [Issue #1](https://github.com/nananek/goronation/issues/1)

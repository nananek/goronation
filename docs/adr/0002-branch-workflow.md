# 0002. ブランチ運用 (develop を base とし、main は保守者が反映する)

- 状態: 採用
- 日付: 2026-09-25
- 関連: `.github/workflows/main-source-guard.yml`
- 注記: 番号 0002 は、0001 が別 PR (m0-repo-foundation) にあるための連番である。

## 状況

- main は、保守者が区切りの良いところで反映した状態だけを持つブランチにしたい。日々の統合先は別に要る。
- エージェントが main や develop へ直接 push したり、main へ PR を出したりできると、この状態を保てない。

## 決定

### 1. ブランチと merge の方式

- `develop` を統合ブランチとし、PR の base にする。develop へは squash merge だけを使う。
- 作業ブランチは develop から切り、develop へ PR を出す。
- `main` へは、保守者が区切りごとに develop から merge commit で反映する。

### 2. エージェントがしないこと

- main と develop へ直接 push する。
- main への PR を出す。main への PR を merge する。
- force push をする。

### 3. main 向けの PR は develop からだけ

main を base とする PR は、同じ repo の `develop` を head とするものだけを許す。これを workflow `main-source-guard` (job 名 `only-from-develop`) が検査する。

### 4. リポジトリ設定は保守者が行う

次は保守者がリポジトリの設定で行う。**コードでは強制されない。**

- ruleset: develop は squash のみ、main は merge commit のみを許可する。
- required check: main-source-guard の job `only-from-develop`。
- 既定ブランチ。

## 帰結

- GitHub には PR の取り込み元ブランチを制限する標準の設定が無いため、workflow による検査で代替する。
- guard は `pull_request_target` で動くので、base 側 (main) の workflow 定義が使われる。PR 側から検査自体を書き換えられない。
- その代わり、main に workflow が入って初めて有効になる。最初の develop から main への merge の後に、保守者が required check に登録する。
- develop に入った変更は、保守者が反映するまで main に出ない。反映の手間は保守者が負う。

## 代替案

### main に直接 PR する運用 (却下)

main と develop の役割を分けられない。日々の統合先と、保守者が区切りごとに反映する先が、同じブランチになってしまう。

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

### 3. main 向けの PR は develop からだけ

main を base とする PR は、同じ repo の `develop` を head とするものだけを許す。これを workflow `main-source-guard` (job 名 `only-from-develop`) が検査する。

### 4. リポジトリ設定は保守者が行う

次は保守者がリポジトリの設定で行う。**コードでは強制されない。**

- ruleset: develop は squash のみ、main は merge commit のみを許可する。
- ruleset (develop と main): PR を必須にし (Require a pull request before merging)、bypass list は空にする。保守者も PR の merge で変更を入れるので、通常の作業は妨げられない。エージェントが保守者と同じアカウントで push しても、直接 push は止まる想定である。設定で担保する対象は決定 2 の「直接 push の禁止」だけで、設定が無い間は約束にすぎない。PR の作成と merge を制限する設定は、この一覧に無く、「main への PR を出さない・merge しない」は運用の約束である (エージェントの資格情報での PR の制限は、egress で行う予定。Issue #1)。
- 上の ruleset について、docs (Available rules for rulesets) は「対象ブランチへのすべての変更を PR に関連づけることを求められる」と述べる。しかし、bypass できない actor の直接 push が拒否されることと、bypass list が空なら管理者を含めて誰も bypass できないことは、原文で明記を確認できておらず**未検証**である。設定後に、保守者が同じアカウントで、ruleset の対象に含めたテスト用のブランチへ直接 push し、拒否されることを確かめる。
- required check: main を対象とする設定にだけ、job 名 `only-from-develop` (workflow `main-source-guard` の job) を登録する。develop には登録しない (この workflow は base が main の PR でしか動かず、`branches` で skip された workflow の check は pending のままになり、merge が塞がれる。docs の Troubleshooting required status checks の「Handling skipped but required checks」による)。required check は workflow を区別しない (docs の Troubleshooting rules の「Troubleshooting required status checks」は「workflow、matrix、event の種類を考慮しない」と述べる)。job 名 `only-from-develop` を、他の workflow で使わない。
- 既定ブランチは `develop` とする (設定済み)。guard は既定ブランチの定義で動くので、guard が develop に入った時点で有効になり、最初の develop から main への PR も検査される (「帰結」を参照)。

## 帰結

- GitHub には PR の取り込み元ブランチを制限する標準の設定が無いため、workflow による検査で代替する。
- guard は `pull_request_target` で動くので、PR の head ではなく、リポジトリの**既定ブランチ**の workflow 定義が使われる (base ブランチの定義ではない。docs の Events that trigger workflows と、2025-12-08 から有効の changelog による: https://github.blog/changelog/2025-11-07-actions-pull_request_target-and-environment-branch-protections-changes/)。そのため、main 向けの PR の中身では、その PR に対する検査を書き換えられない。guard の定義は、develop に merge された内容で決まる。
- その代わり、guard は develop に入って初めて有効になる。required check への登録は、有効になった後に保守者が行う。
- guard 自身の変更を含む develop 向けの PR では、guard は動かない (base が main ではないため)。`.github/` の変更は、保守者が目で確認する。
- `edited` を trigger に含めるのは、PR の base が後から main に変更された場合も検査するためである。docs の Webhook events and payloads は `edited` を「title か body の編集、または base ブランチの変更」と述べ、Events that trigger workflows は activity type の意味をそこに委ねている。ただし、base の変更でこの workflow が実際に起動し、`branches` が新しい base で評価されることは、実機で確かめておらず**未検証**である。起動しなくても、required check は pending のままになり、merge は塞がれる (決定 4 と同じ)。
- guard は事故防止であり、悪意ある PR 作成者への防壁ではない。別の workflow が同名の job を success で報告した場合に required check が満たされうるかは、**未検証**である。
- develop に入った変更は、保守者が反映するまで main に出ない。反映の手間は保守者が負う。

## 代替案

### main に直接 PR する運用 (却下)

main と develop の役割を分けられない。日々の統合先と、保守者が区切りごとに反映する先が、同じブランチになってしまう。

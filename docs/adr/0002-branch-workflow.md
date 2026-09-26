# 0002. ブランチ運用 (develop を base とし、main は保守者が反映する)

- 状態: 採用
- 日付: 2026-09-25 (2026-09-26 に追記)
- 関連: `.github/workflows/main-source-guard.yml`

## 状況

- main は、保守者が区切りの良いところで反映した状態だけを持つブランチにしたい。
- エージェントが main や develop へ直接 push し、main へ PR を出せると、この状態を保てない。

## 決定

### 1. ブランチと merge の方式

- `develop` を統合ブランチとし、PR の base にする。develop へは squash merge だけを使う。
- 作業ブランチは develop から切り、develop へ PR を出す。
- `main` へは、保守者が区切りごとに develop から merge commit で反映する。
- その merge commit は main にしか無いので、保守者が反映した直後に develop へ戻す。develop に他の変更が入る前に、fast-forward で行う (入ると fast-forward にならない)。develop への直接 push (force なし) で、決定 2 の例外である。

### 2. エージェントがしないこと

- main と develop へ直接 push する。
- main への PR を出す。main への PR を merge する。

### 3. main 向けの PR は develop からだけ

main を base とする PR は、同じ repo の `develop` を head とするものだけを許す。これを workflow `main-source-guard` が検査する。

### 4. リポジトリ設定は保守者が行う

次は保守者がリポジトリの設定で行う。**コードでは強制されない。**

- ruleset は develop 用と main 用の 2 つに分ける。merge 方式は ruleset ごとの設定なので (docs の Available rules for rulesets)、develop 用は squash のみ、main 用は merge commit のみを許可する。どちらも PR を必須にし (Require a pull request before merging)、bypass list は空にする。保守者も PR の merge で変更を入れる。
- 決定 1 の戻す操作のときだけ、保守者が develop 用の ruleset を一時的に無効にし (bypass list は空のまま)、終えたらすぐ有効に戻す。無効の間は、develop の規則が効かない (docs の About rulesets は、Disabled を「enforce されない」と述べる)。
- 設定で担保する対象は、決定 2 の「直接 push の禁止」だけで、設定が無い間は約束にすぎない。エージェントが保守者と同じアカウントで push しても、直接 push は止まる想定。「main への PR を出さない・merge しない」は設定では担保されず、運用の約束である (将来 egress で PR を絞る予定。Issue #1)。
- 未検証: PR を必須にする規則が、bypass できない actor の直接 push を拒否すること。bypass list が空なら、管理者を含めて誰も bypass できないこと。docs の原文に明記が無い。設定後に、保守者が同じアカウントで、ruleset の対象に含めたテスト用のブランチへ直接 push し、拒否されることを確かめる。
- required check: `ci.yml` の job `base` と `bwrap` は develop 用と main 用の両方に、`main-source-guard` の job `only-from-develop` は main 用にだけ登録する。`ci.yml` の `pull_request` には `branches` の指定が無く、base ブランチを限定しない。`main-source-guard` は base が main の PR でしか動かず、develop 用に登録すると develop 向けの PR が塞がれる。workflow が skip される (起動しない) と、required check は pending のままで merge が塞がれるためである (docs の Troubleshooting required status checks の「Handling skipped but required checks」)。登録は、対象の workflow (`ci.yml`、`main-source-guard`) が develop に入ってから行う。
- required check は workflow を区別しない (docs の Troubleshooting rules は「workflow、matrix、event の種類を考慮しない」と述べる)。登録する job 名を、他の workflow で使わない。expected source (更新を受け付ける app) は GitHub Actions に固定する (docs の Available rules for rulesets は、app を選べると述べる。選択肢に GitHub Actions が出ることは未検証)。
- 既定ブランチは `develop` とする (設定済み)。guard が develop に入った時点で有効になり、最初の develop から main への PR も検査される。

## 帰結

- GitHub には PR の取り込み元ブランチを制限する標準の設定が無いため、workflow で検査する。
- guard は `pull_request_target` で動くので、PR の head ではなく、**既定ブランチ**の workflow 定義が使われる (base ブランチの定義ではない。docs の Events that trigger workflows と、2025-12-08 から有効の changelog「Actions pull_request_target and environment branch protections changes」による)。そのため、main 向けの PR の中身では、その PR に対する検査を書き換えられず、guard の定義は develop に merge された内容で決まる。
- guard 自身の変更を含む develop 向けの PR では、guard は動かない。`.github/` の変更は、保守者が目で確認する。
- `edited` を含めるのは、base が後から main に変更された PR も検査するため。docs の Webhook events and payloads は `edited` に「base ブランチの変更」を含めると述べる。base の変更でこの workflow が起動し、`branches` が新しい base で評価されるかは、実機で**未検証**である。起動しなくても、同名の成功の報告が無い限り、required check は pending のままで merge は塞がれる (次の項も参照)。
- guard は事故防止であり、悪意ある PR 作成者への防壁ではない。別の workflow が同名の job を success で報告した場合に required check が満たされうるかは、**未検証**である。
- 反映と戻す手間は、保守者が負う。

## 代替案

### main に直接 PR する運用 (却下)

main と develop の役割を分けられず、日々の統合先と、保守者が反映する先が同じになる。

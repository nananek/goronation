# 0043. opencode の question の tool は、session の permissions で承認の段を省く (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0021 (決定 5 を置き換える)・0040

## 状況

opencode 2.0.20 の question の tool は、先に permission (action `question`・resources `["*"]`) を求め、承認のあとに form (ADR 0040) が出る。利用者には 2 段 (tool の承認 → 質問への回答) になる。`*` の承認は、見せるものが無く、人間への問いかけ自体が関門である (採取: `question-single`)。ADR 0021 決定 5 は、`question`・`form` を `agent.frame` にして返答しない、とした。

## 決定

1. **ADR 0021 決定 5 を置き換える**: form は ADR 0040 の `form.requested` として扱う。`agent.frame` にしない。
2. **session 作成時の `permissions` を、`[{"*" ask}, {"question" allow}]` の順にする** (ADR 0021 決定 1 のとおり、API を呼べる者だけが変えられる)。実測 (2.0.20・`question-rule-allow`): あとの規則が勝ち、question の `permission.asked` は出ず、`form.created` が直接出る。順序を逆にすると、あとの `*` が勝って、`permission.asked` が出る (採取で確認)。
3. **採らない**: アダプタ・transport が `permission.asked` に自動で `once` を返す案。承認の返答を、人間の操作なしに作ることになり、「by=policy」の記録も要る。設定で済むものを、実行時の自動応答にしない。
4. **順序が効かない版でも、安全側に倒れる**: 最初に一致した規則が勝つ版なら、`*` の ask が勝って、`permission.asked` が出る (承認の段が戻るだけ。承認を省く方向には、誤らない)。adapter は、`action: question` の `permission.asked` が来たら、通常の承認として扱う (`permission.requested`。ADR 0041 の対応表)。
5. **form は、ほかの tool からも出る** (web 検索の provider の選択)。その tool は、別の permission (action `websearch`) を持ち、承認の画面に出る。form の許可は、question だけにしか、与えない。
6. PR⑥ は、本番の session 作成の本文に、この permissions を入れ、`question-rule-allow` の fixtures と同じ事象 (permission.asked が出ない) を、E2E で確かめる。 あわせて、同じ ruleset で、別の action (shell など) が `permission.asked` になることも確かめる (question だけが省かれ、ほかは承認を要求したまま)。

## 帰結

- 利用者は、question を、1 段 (回答) で受ける。`question` の action は、承認の画面に出ない。
- opencode の版が上がり、規則の評価の順序が変わったら、採取をやり直して確かめる (ADR 0018 の帰結)。

## 代替案

- 承認を、利用者に見せる: 見せるものが無い (`*`) 承認で、2 回押させる。
- 自動応答: 決定 3。

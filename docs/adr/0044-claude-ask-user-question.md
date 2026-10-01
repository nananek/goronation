# 0044. claude の AskUserQuestion は form にし、回答は updatedInput.answers で返す (M2)

- 状態: 採用 (2026-10-01 に追記: 決定 1〜4 のうち、アダプタ・Conversation・API は PR⑤b-1 で実装した (ADR 0046・0047)。画面の form は PR⑤b-2)
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0010・0040・0042、`spec/testdata/golden/claude/ask-user-question-*.jsonl`

## 状況

claude の AskUserQuestion は、`--permission-prompt-tool stdio` で、`can_use_tool` の control_request として来る (`requires_user_interaction: true`・`display_name`・input は `questions[{question, header, multiSelect, options[{label, description}]}]`)。goronation は、これを通常の `permission.requested` にして、Approve で、保持した input だけを `updatedInput` に入れて許可する。実測 (claude 2.1.286): この応答では、tool_result は「The user did not answer the questions.」で、`tool_use_result.answers` は空。**回答が届かないので、質問は機能していない** (`ask-user-question-unanswered`)。

## 決定

1. **AskUserQuestion の `can_use_tool` は、`form.requested` (kind `question`) にする** (ADR 0040 決定 4)。`permission.requested` にしない。検出は、`tool_name` が `AskUserQuestion` のとき (`requires_user_interaction` が true でも、tool 名が別なら、通常の承認)。
2. **回答は、allow の `updatedInput` に、保持した input と、`answers` を足して返す**: `answers` は、質問の文 → 回答の文字列。アダプタは、質問の文を、要求時に保持する (key `q0`… → 質問の文)。**複数選択は、`, ` で連結した 1 つの文字列** (SDK の型 `Record<string,string>` に合う形)。2.1.286 は、配列も受けたが (`answers` に配列のまま入る)、頼らない。選択肢に無い自由記述 (Other) は、そのまま文字列で、claude が受ける (tool_result の文言が違うだけ)。実測: `ask-user-question-single`・`-multi`・`-custom`。
3. **取り消しは deny** (message は固定の文)。tool は is_error で終わる (`ask-user-question-deny`)。`control_cancel_request` (エージェントの取り下げ) は、今までどおり `form.resolved` (by=agent・cancelled)。
4. **input は、クライアントから受けない** (今までの原則のまま): updatedInput は、保持した input と、検査を通った回答 (`FormResolve.Validate`) だけから作る。質問の文・options は、保持した値。
5. **修正は、PR⑤b** (`agent/claude`・`Conversation` の `form.resolve`・UI の form の描画と回答の送信)。この ADR は、根本原因と形を固定し、`spec/v0` の適合テストが、実フレームで、回答の写し (`answers`) を固定する。

## 帰結

- 質問が機能する。claude と opencode で、同じ UI・同じ検査になる。
- claude の版が上がったら、`ask-user-question-*` を採り直して確かめる (採取は 2.1.286。既存の fixtures は 2.1.284)。
- サブエージェントの AskUserQuestion の扱いは、未採取 (ADR 0045)。
- **限界**: 複数選択の `, ` 連結は、値に `, ` を含む選択肢や、自由記述 (Other) の値では、claude から見て曖昧になる (回答の文字列が、どの選択に分かれるか、claude は区別しない)。回答の検査 (`FormResolve.Validate`) は、値の配列を持つので、goronation 側の記録は曖昧にならない。

## 代替案

- allow に answers を足さず、質問の文を、prompt として、次のターンに送る: tool が、回答なしで終わり、会話が汚れる。
- 回答を配列で返す: 動くが、型が文字列の SDK から外れる。

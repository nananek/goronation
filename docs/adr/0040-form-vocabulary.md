# 0040. 人間への入力の要求は、質問でなく form (フィールドの一覧) として表す (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0015・0021・0041・0042・0043・0044、`spec/v0/form.go`

## 状況

claude の AskUserQuestion と opencode の form (question の tool・web 検索の provider の選択) は、どちらも「エージェントが人間にフィールドを入力させる」仕組みで、質問専用ではない。opencode の form は `metadata.kind` で種類が分かれる (採取: `question`・`websearch.provider`)。ADR 0021 決定 5 は、これらを `agent.frame` にして返答しない、とした (ADR 0043 が置き換える)。claude では、質問を Approve しても答えが届かず、tool が「回答なし」で終わる (ADR 0044)。

## 決定

1. **語彙を `spec/v0` に足す** (追加だけで、既存の語彙は変えない)。イベント `form.requested` (data は `FormRequested`)・`form.resolved` (request_id・by・outcome・answer)、コマンド `form.resolve` (`FormResolve`: request_id・outcome・answer・content_hash)。
2. **フィールド**: key (form の中で一意)・title・description・type・options (label・value・description)・custom (options 以外の入力を許す)・required。type は `select` (1 つ選ぶ)・`multiselect` (0 個以上)・`text` (自由な 1 つの文字列)。回答は、select・text が文字列、multiselect が文字列の配列。
3. **kind はエージェントが決める文字列** (`question` だけ定数)。UI は kind で挙動を変えず、フィールドの一覧として描く (知らない kind も安全に表示できる)。
4. **対応**: claude の AskUserQuestion → `question`・質問ごとに key `q0`…・multiSelect が true なら multiselect・custom は常に true (claude は Other を許す)。opencode の `form.created` → `form.id` が request_id・`metadata.kind` が kind・`metadata.tool.id` が call_id・type `string` は options があれば select、無ければ text・`multiselect` はそのまま。
5. **エージェントの文は敵対入力**: title・description・label は検証されていない自己申告で、UI は ADR 0015 の印で表示し、「回答はエージェントに渡る」ことを示す。秘密の入力欄 (password 型など) は、語彙に無い。上限: フィールド 16・選択肢 32・title と label 200 字・description 1,000 字・回答 1 つ 4,000 字。超える要求・形の不正な要求は、アダプタが `agent.frame` にする。
6. **回答は、保持した要求に対してだけ検査する** (`FormResolve.Validate`): キーは要求のフィールドだけ・型が合う・custom でなければ値は options のどれか・required は空でない・重複なし。通らなければ error で、何も書かない。要求の決着は 1 つ (1 回限り)。`cancelled` は answer を持たない。
7. **UI が対応する前に、form を出さない**: M1.5 の UI は `form.requested` を描けない。アダプタが出し始めるのは、UI の対応と同じ PR (PR⑤b・PR⑥)。

## 帰結

- 質問は form の 1 つの kind になり、claude と opencode で、同じ UI・同じ検査を使える。3 つ目のエージェントも、同じ形に写せばよい。
- 回答の値がエージェントに渡るので、利用者が秘密を入力しないよう、UI の文言で促す (強制はできない)。
- form の応答も、ADR 0042 の内容ハッシュの対象。

## 代替案

- 質問専用の語彙: websearch の provider の選択など、質問でない form を表せない。
- `permission.requested` に統合: 承認 (許可・拒否) と入力 (値を返す) は、結果の形が違う。承認の関門 (ADR 0016) に、値の検査が混ざる。

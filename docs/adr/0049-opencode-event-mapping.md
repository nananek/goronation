# 0049. opencode の SSE は、表のとおりに v0 に写し、承認できない形・未知の type は落とさず agent.frame か TypeError にする (M2)

- 状態: 採用
- 日付: 2026-10-01
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0022・0040・0041・0043・0045、`agent/opencode`、`spec/testdata/golden/opencode-serve`

## 状況

PR④ が、opencode 2.0.20 の SSE を 43 種の type・19 場面で採った。`agent/opencode` は、その 1 行 (`data:` の JSON) を `[]v0.Envelope` にする (ADR 0022 決定 2)。イベントは、`location.directory`・`projectID`・`slug`・provider の URL・`response.body` を含み、`data` は UI・API に出る。

## 決定

1. **扱いは `eventKinds` の表が正** (`classify_test.go` が、golden の全 type が表にあり、表の全 type が golden にあることを確かめる)。表に無い type は `agent.frame` (lenient)。
2. **出さない** (空の列・状態も持たない): `*.updated`・`server.connected`・`session.inbox.*`・`execution.started`・`instructions.updated`・`renamed`・`step.started`・`step.streamed`・`step.failed`・`tool.input.ended`・`text.started`・`text.delta`・`usage.updated`・`shell.*`。`tool.input.started` は tool 名を覚えるだけ。
3. **出す**: root の最初の `session.created` (parentID 無し) → `session.started` (agent・agent_session・cwd・version)。`text.ended` → `message.text` (空は出さない)。`tool.called` → `tool.call` (pending)。`tool.progress`・`success`・`failed` → `tool.update`。`step.ended` → `usage` (scope=step)。`permission.asked` → `permission.requested`、`form.created` → `form.requested`。`retry.scheduled`・`synthetic` → `agent.frame`。
4. **usage は `step.ended` から**: `usage.updated` は累積で、両方出すと二重に数える。窓の上限は出ない。**delta は出さない** (`message.delta` は予約のまま。UI が描けるようになるまで。変えるときは新しい ADR)。**model は出さない** (session.started のあとに分かる)。
5. **終わりは root の `execution.*` だけ**: `succeeded` → end_turn・`interrupted` → cancelled・`failed` → `error` + error。子の終わりは `turn.completed` にしない (ADR 0045 決定 4)。終わった session の未決の要求・form は、先に by=agent・cancelled で閉じる (`interrupt-pending-permission` は replied が来ない)。
6. **session の絞り込み**: root でも子でもない session のイベントは `agent.frame` (ほかの利用者の session)。子は、親が root か既知の子のときだけ (64 まで)。子のイベントは `Origin{id: 子の session, parent: 起動した tool の call_id}`。
7. **要求の決着はちょうど 1 つ**: EncodeCommand が返したもの (ADR 0050) の `replied`・`cancelled` は、合成した決着と重複するので出さない。未決のまま来たものは by=agent。同じ ID の再要求は、決着の後も `agent.frame` (先の内容を、差し替えさせない)。未決は 64 まで、見た ID は SHA-256 で 2^17 まで。超えた・形が不正な要求は、承認できず、`TypeError` で知らせる (opencode は待ち続ける)。
8. **漏れの防止**: data は語彙が必要とする値だけ。`error` は message (4,000 字まで)・status・retryable で、url・ヘッダ・response.body は raw にだけ。`leak_test.go` が golden の全イベントで確かめる。

## 帰結

- 実機で得た 43 種の分類が、テストで固定される。版が上がって type が増えたら、採取をやり直す (ADR 0018)。
- 子のイベントは Origin つきで出るので、UI は帰属を出せる (PR⑤b・PR⑥)。
- root は SSE の最初の session.created で決まる。transport が作った session と一致することは、PR⑥ が確かめる。

## 代替案

- `usage.updated` を出す: 累積なので、UI が差分を取る必要がある。
- delta を出す: UI が描けない。
- 形が不正な要求を `agent.frame` だけにする: 人間が却下できず、opencode が待ち続ける。

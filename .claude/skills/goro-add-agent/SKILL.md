---
name: goro-add-agent
description: goro run に、新しいエージェント (claude・opencode の次) を足す手順と、これまでに踏んだ落とし穴。「エージェントを足す」「新しい --agent」「profile を書く」「codex や copilot を檻で動かす」などのときに使う。
---

# goro run に、新しいエージェントを足す

## 原則

- **最初のエージェントを特別扱いしない**。エージェントごとの違いは、`cmd/goro/agent.go` の profile 表のデータだけ (「claude なら A・opencode なら B」の分岐をコードに書かない)。名前から導出するもの (`GORO_<NAME>`・檻の中の path・状態のディレクトリ) も、表に書かない。
- 状態は `<state>/agents/<名前>/{home,login-work,login-run}`。**フラグを足さない** (`--bin` と `GORO_<NAME>` で足りる)。設計の正は Issue #1 と `cmd/goro/doc.go`。

## 手順

1. **使い捨ての spike** (コミットしない。scratchpad で行う)。`--bin` で任意の実行ファイルを檻で動かし、`--allow` を 1 つずつ足して、次を実測する。
   - 起動・ログイン・1 往復に要る宛先 (既定の許可は、これだけにする。拒否された宛先は、要るかを確かめてから足す)。
   - 通信を止める環境変数。**名前の実在 (binary の文字列) と、効くか (`egress.log` の拒否が消えるか) は別**。存在しても効かないものが、既にあった。変数ごとに測るか、測っていないと書く。
   - HOME のどこに書くか / `HTTPS_PROXY` を尊重するか / ログインが檻の中で完結するか (コードを貼る方式か、ホストの localhost に戻す方式か。後者は届かない)。
   - **初回の落とし穴**: ログインの直後、次の起動が onboarding からやり直しにならないか (何のキーが判定するか)。claude で踏んだ (`.claude.json` の `hasCompletedOnboarding`)。
   - 要る道具 (`rg`・`git`・`bash`)。無いなら、ホストに入れる (檻の `/usr` に見える)。
   - 実際の資格情報が要る確認は、利用者に頼む。資格情報は、読まない・貼らない・ログに残さない。確かめられなかった部分は、PR 本文に「未確認」と書く。
2. **profile を 1 行足す**: `agents` 表に、name・bin (PATH で探す名前)・exeExample・env (通信を止めるもの)・hosts (`egress.<Name>Hosts()`。呼ぶたび新しい slice を返す)・loginArgs・loginGuide・loginUsage・exitHint・denyNotes を書く。各フィールドの意味は、`agentProfile` の doc comment が正。
3. **ホスト側の実際の設定・資格情報の dir を、`sandbox/bwrap/argv.go` の `homeSecrets` に足す** (opencode の `.local/share/opencode`・`.config/opencode` が例)。足りないと、利用者の実際の資格情報を、檻に bind できる。テストは `sandbox/bwrap/argv_test.go` の、opencode の例と同じ形で足す。
4. **テスト**: `TestRunThirdAgent` (表に足すだけで通る作りになっている。通らなければ、分岐を書いた印)・golden (`TestCageSpecGoldenOpenCode` と同じ形で、argv を固定する)・`GORO_REQUIRE_BWRAP=1 make check`。変異 (分岐・許可・環境変数を壊して、テストが落ちるか) も見る。
5. **攻撃者視点のレビューを 1 回**受ける (`attack-review`)。檻の Spec・許可宛先・環境変数・エージェント間の隔離 (不変条件 I2) に触るので、省かない。
6. **PR 本文**: 「利用者が今できること」(login → repo → export の手順) と、未確認の部分 (実際の資格情報が要るもの) を分けて書く。

## 踏んだ落とし穴 (同じ形を疑う)

- ログインの作業ディレクトリ・run dir を、エージェント間で共有した: 低権限側の檻が置いた設定 (`opencode.json` など) を、高権限側 (認証情報を持つ HOME) の檻が読んで動かした (B1)。状態は、必ず名前ごとに別にする。
- onboarding の完了が保存されず、ログインしたのに毎回ログイン画面が出た (claude)。
- PATH のエージェントがラッパースクリプトで、檻の中で実体が見えず 127 で終わった (Arch の claude)。
- 環境変数の名前が存在しても、止めたい通信が止まらなかった。egress の拒否一覧で、効果を測る。

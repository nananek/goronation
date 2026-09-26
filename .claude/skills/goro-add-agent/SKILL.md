---
name: goro-add-agent
description: goro run に、新しいエージェント (claude・opencode の次) を足す手順と、これまでに踏んだ落とし穴。「エージェントを足す」「新しい --agent」「profile を書く」「codex や copilot を檻で動かす」などのときに使う。
---

# goro run に、新しいエージェントを足す

## 原則

- **最初のエージェントを特別扱いしない**。エージェントごとの違いは、`cmd/goro/agent.go` の profile 表のデータだけ (「claude なら A・opencode なら B」の分岐をコードに書かない)。名前から導出するもの (`GORO_<NAME>`・檻の中の path・状態のディレクトリ) も、表に書かない。
- 状態は `<state>/agents/<名前>/{auth,homes,login-home,login-work,login-run}` (`auth` = 認証情報だけ・全 repo で共有、`homes/<repo のキー>` = repo ごとの HOME)。**フラグを足さない** (`--bin` と `GORO_<NAME>` で足りる)。設計の正は Issue #1 と `cmd/goro/doc.go`。

## 手順

1. **使い捨ての spike** (コミットしない。scratchpad で行う)。`--bin` で任意の実行ファイルを檻で動かし、`--allow` を 1 つずつ足して、次を実測する。**`--state-dir` は、使い捨ての短い path にする**: `--agent` は表の中の名前しか取れないので、spike は「既存の名前 + `--bin`」の形になる。既定の状態のままだと、その名前の認証情報 (`auth/`) と repo の HOME (`homes/`) が、まだ信頼していない実行ファイルの檻に見える。
   - **原則: エージェントの TUI を解釈・模倣しない**。ログインと操作は、エージェント自身の画面で行い、goro は端末に直結する (画面の手順を goro や skill に写すと、版が変わって嘘になる。opencode 2 系で、実際に食い違った)。
   - 起動・ログイン・1 往復に要る宛先 (既定の許可は、これだけにする。拒否された宛先は、要るかを確かめてから足す)。
   - 通信を止める環境変数。**名前の実在 (binary の文字列) と、効くか (`egress.log` の拒否が消えるか) は別**。存在しても効かないものが、既にあった。変数ごとに測るか、測っていないと書く。
   - HOME のどこに書くか / `HTTPS_PROXY` を尊重するか (檻の中のプロセス同士の loopback 通信が、proxy に流れて拒否されないか。goro init が、loopback を `NO_PROXY` で外す) / ログインが檻の中で完結するか (ホストのブラウザの localhost に戻る方式は、檻に届かない)。
   - **認証情報の書き方と置き場** (共有のしかたが決まる): その場で書くか (同じ inode)・一時ファイル + rename か (inotify で観測)。置き場を環境変数で移せるか (binary の文字列を探し、移せたら、読み・書き・更新のロックが、そこに閉じるかを測る = `creds.env`)。移せず、その場で書くなら、HOME からの symlink (`creds.linkDir`・`files`)。rename で書くものに symlink は使えない (HOME だけの普通のファイルに変わり、共有から外れる)。refresh token が回転すると、認証情報のコピーは、片方の更新で、もう片方が切れる (共有を 1 か所にする理由)。実サーバーの回転は、実物の資格情報が要るので、未確認と書く。
   - **種 (`seed`) の最小**: repo ごとの新しい HOME で、初回の設定を飛ばすのに要る最小のファイル。アカウントの表示情報のように、起動のたびに認証情報から作り直されるものは、種にも共有にも入れない (別のアカウントで実測する)。
   - **初回の落とし穴**: ログインの直後、次の起動が onboarding からやり直しにならないか (何のキーが判定するか)。claude で踏んだ (`.claude.json` の `hasCompletedOnboarding`)。
   - 要る道具 (`rg`・`git`・`bash`)。無いなら、ホストに入れる (檻の `/usr` に見える)。
   - 実際の資格情報が要る確認は、利用者に頼む。資格情報は、読まない・貼らない・ログに残さない。確かめられなかった部分は、PR 本文に「未確認」と書く。
2. **profile を 1 行足す**: `agents` 表に、name・bin (PATH で探す名前)・exeExample・env (通信を止めるもの)・hosts (`egress.<Name>Hosts()`。呼ぶたび新しい slice を返す)・creds・seed・loginArgs・loginUsage (理由・制約・API キーの取得 URL だけ。画面の項目名・手順は書かない)・exitHint・resumeUsage・denyNotes (宛先の後ろに添える短い一言) を書く。`--login` の案内文 (`loginGuide()`) は、エージェント共通で、終了操作だけを添える。各フィールドの意味は、`agentProfile` の doc comment が正。
3. **ホスト側の実際の設定・資格情報の dir を、`sandbox/bwrap/argv.go` の `homeSecrets` に足す** (opencode の `.local/share/opencode`・`.config/opencode` が例)。足りないと、利用者の実際の資格情報を、檻に bind できる。テストは `sandbox/bwrap/argv_test.go` の、opencode の例と同じ形で足す。
4. **テスト**: `TestRunThirdAgent` は、テスト用の偽の第 3 profile で、エージェントによらない共通の部分 (表に足すだけで動くこと) を見る。新しい実 profile の宛先・環境変数・argv は見ないので、`egress/hosts_test.go` の宛先のテストと、golden (`TestCageSpecGoldenOpenCode` と同じ形で、argv を固定する) で見る。認証情報の共有は、`TestRunCredentialsViaSymlink` (第 3 profile の creds・seed のデータだけで動く) と、`TestRealProfilesCredentialsAndSeed` (実測の値) で見る。あわせて `GORO_REQUIRE_BWRAP=1 make check`。変異 (分岐・許可・環境変数を壊して、テストが落ちるか) も見る。
5. **攻撃者視点のレビューを 1 回**受ける (`attack-review`)。檻の Spec・許可宛先・環境変数・エージェント間の隔離 (不変条件 I2) に触るので、省かない。
6. **PR 本文**: 「利用者が今できること」(login → repo → export の手順) と、未確認の部分 (実際の資格情報が要るもの) を分けて書く。

## 踏んだ落とし穴 (同じ形を疑う)

- ログインの作業ディレクトリ・run dir を、エージェント間で共有した: 低権限側の檻が置いた設定 (`opencode.json` など) を、高権限側 (認証情報を持つ HOME) の檻が読んで動かした (`TestRunLoginWorkNotSharedAcrossAgents`)。状態は、必ず名前ごとに別にする。
- HOME をエージェントで 1 つ共有した (repo をまたいで履歴・メモリ・trust が混ざり、信頼できない repo の檻から、別の repo の会話履歴が読めた)。認証情報を各 HOME にコピーする案は、更新で片方が切れる。認証情報だけを `auth/` の 1 か所から渡し、HOME は repo ごとに分ける (`TestRunHomePerRepo`)。
- onboarding の完了が保存されず、ログインしたのに、次の起動でまたログインを求められた (claude)。
- PATH のエージェントがラッパースクリプトで、檻の中で実体が見えず 127 で終わった (Arch の claude)。
- 環境変数の名前が存在しても、止めたい通信が止まらなかった。egress の拒否一覧で、効果を測る。

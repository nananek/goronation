---
name: goro-run
description: goro run で AI エージェント (claude・opencode) を、ネットワークの無い bwrap の檻の中で動かす手順と、動かないときの切り分け。「goro run」「檻でエージェントを動かす」「ログインしたのに再度ログインを求められる」「拒否された宛先」「--allow」などが話題に出たときに使う。
---

# goro run の使い方とトラブルシュート

エージェントを、ネットワークの無い bwrap の檻の中で、ホストの repo の private clone の上で動かす。設計と限界の正は `cmd/goro/doc.go`、フラグの正は `goro run -h` (ここに写さない。ここは、手順と落とし穴だけ)。

## 前提

- Linux と bwrap (`/usr/bin/bwrap`)。ホストに git と、エージェントの **native の実行ファイル** (スクリプトのラッパーは不可)。
- `cat /proc/sys/dev/tty/legacy_tiocsti` が `0`。この sysctl は kernel 6.2 で追加されたもので、既定は kernel の設定による。0 でない (読めない) と、端末に直結する起動は断られる。
- opencode を使うなら、ホストに ripgrep (`rg`。檻の `/usr` に見える)。入れるのは利用者。

## 最小の流れ

```
make build                                   # bin/goro ができる
bin/goro run --agent claude --login          # 初回。--agent を省くと claude
bin/goro run --agent claude --repo ~/work/foo
bin/goro export ID                           # bundle と、取り込みの git fetch を表示する。自分の repo で、そのまま実行する
bin/goro sessions                            # 一覧。再開は goro run --session ID
```

- **ログインは、利用者が、エージェント自身の画面で行う (画面の指示に従うのは、利用者)**。エージェントは代行せず、資格情報を読まない・貼らない・ログに残さない。goro は端末に直結し、出力を解釈しない (画面の中身・手順は、エージェントの版で変わるので、ここに書かない)。
- 状態は `<state>` (既定 `~/.local/state/goro`。`--state-dir` で変える): ログイン状態 (認証情報) は `agents/<名前>/auth` (エージェントごとに 1 つ。全 repo の檻で共有)、会話の履歴・メモリ・trust の承認は `agents/<名前>/homes/<repo のキー>` (repo ごと)、セッションは `sessions/<id>/{clone,run,export}`。
- **HOME は repo ごと**: 別の repo の履歴は見えず、同じ repo のセッションは共有する (メモリが続く)。ログインは 1 回で全 repo に効き、再ログイン・更新も全 repo に伝わる。repo のキーは、実 path (symlink を解決) から作る。`--session` は、作ったときの repo の HOME で動く。
- clone されるのは**コミット済みの内容だけ**。元の repo の作業ツリー・`.git`・`~/.ssh`・エージェントの設定 (`~/.claude` など)・環境変数は、檻から見えない。
- ホストは clone の中で git を実行しない。成果は、`goro export` が出した `git fetch` を、利用者が自分の repo で実行して取り込む。
- `--session` は、そのセッションを作ったエージェントで動く (別の `--agent` は断られる)。

## トラブルシュート (実機で出たもの)

1. **「…はスクリプトで、檻の中では動かない」と断られる (古い goro では、終了コード 127 と `No such file or directory`)**: PATH のエージェントがラッパースクリプト (Arch の claude など)。檻には、スクリプトだけが見え、それが呼ぶ実体が見えない。実体を `--bin PATH` か環境変数 `GORO_<名前の大文字>` (例: `GORO_CLAUDE`) で指す。
2. **ログインした直後なのに、次の起動で、またログインを求められる (claude)**: `claude auth login` は、onboarding の完了を保存しない。そのため、`--login` は素の対話起動にしてある。古い共有の HOME (`<state>/home`・`agents/<名前>/home`) が残っている人は、履歴・メモリが repo 間で混ざっているので、それを消して `--login` をやり直す (消すのは利用者。こちらで消さない)。
3. **終了後の「拒否された宛先」**: 既定の許可 (エージェントごと。`goro run -h`) 以外は拒否される。一言が添えられるもの (正は `cmd/goro/agent.go` の `denyNotes`) は、添えられたとおりにする: opencode の `registry.npmjs.org`・`models.opencode.ai` は「許可不要」、`github.com` は「ホストに rg を入れる」(項目 5)。一言が無い宛先は、それが無いと起動・ログイン・1 往復ができないと分かってから、`--allow HOST:PORT` で足す。**足すかは、宛先と理由を示して、利用者が決める (エージェントは足さない)**。足す前に、その宛先が何かを確かめる。古い goro では、claude でも `downloads.claude.ai`・`github.com` が出る (今は環境変数で止めている。goro を更新する)。
4. **「TIOCSTI が有効」で起動しない**: `/proc/sys/dev/tty/legacy_tiocsti` が 0 でない。0 にする (`sysctl dev.tty.legacy_tiocsti=0`) のは、利用者。
5. **opencode の grep ツールが失敗する**: 檻の中に `rg` が無く、download しようとして `github.com` が拒否される。ホストに ripgrep を入れる (入れるのは利用者)。`github.com` は許可しない。
6. **「egress の UDS の path が長すぎる (… 上限 107)」**: `--state-dir` を短い path にする。
7. **`--claude` は廃止**: `--bin PATH` を使う (環境変数 `GORO_CLAUDE` などは、そのまま使える)。
8. **「同じセッション (--login なら、同じエージェントのログイン) を、別の goro run が使っている」**: 同じセッション・同じエージェントのログインは、同時に 1 つだけ。先の goro run が終わってから、もう一度実行する。
9. **「このセッションは、HOME を repo ごとに分ける前に作った (使えない)」**: 古いセッション。成果は `goro export` で取り出せる。続きは、`--repo` で新しく作る。

## 限界・安全上の注意

正は、`cmd/goro/doc.go` の「限界」と、`goro run -h` の「限界」。ここには写さない (写すと、実装と食い違う)。この skill の手順を進める前に、そこを読む。

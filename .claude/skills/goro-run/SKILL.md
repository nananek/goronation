---
name: goro-run
description: goro run で AI エージェント (claude・opencode) を、ネットワークの無い bwrap の檻の中で動かす手順と、動かないときの切り分け。「goro run」「檻でエージェントを動かす」「ログインしたのに再度ログインを求められる」「拒否された宛先」「--allow」などが話題に出たときに使う。
---

# goro run の使い方とトラブルシュート

エージェントを、ネットワークの無い bwrap の檻の中で、ホストの repo の private clone の上で動かす。設計と限界の正は `cmd/goro/doc.go`、フラグの正は `goro run -h` (ここに写さない。ここは、手順と落とし穴だけ)。

## 前提

- Linux と bwrap (`/usr/bin/bwrap`)。ホストに git と、エージェントの **native の実行ファイル** (スクリプトのラッパーは不可)。
- `cat /proc/sys/dev/tty/legacy_tiocsti` が `0` (kernel 6.2 以降の既定)。0 でないと、端末に直結する起動は断られる。
- opencode を使うなら、ホストに ripgrep (`rg`。檻の `/usr` に見える)。

## 最小の流れ

```
make build                                   # bin/goro ができる
bin/goro run --agent claude --login          # 初回。--agent を省くと claude
bin/goro run --agent claude --repo ~/work/foo
bin/goro export ID                           # bundle と、取り込みの git fetch を表示する。自分の repo で、そのまま実行する
bin/goro sessions                            # 一覧。再開は goro run --session ID
```

- **ログインは、エージェント自身の画面で行う。画面の指示に従う**。goro は端末に直結し、出力を解釈しない (画面の中身・手順は、エージェントの版で変わるので、ここに書かない)。
- 状態は `<state>` (既定 `~/.local/state/goro`。`--state-dir` で変える): ログイン状態と履歴は `agents/<名前>/home`、セッションは `sessions/<id>/{clone,run,export}`。
- clone されるのは**コミット済みの内容だけ**。元の repo の作業ツリー・`.git`・`~/.ssh`・エージェントの設定 (`~/.claude` など)・環境変数は、檻から見えない。
- ホストは clone の中で git を実行しない。成果は、`goro export` が出した `git fetch` を、利用者が自分の repo で実行して取り込む。
- `--session` は、そのセッションを作ったエージェントで動く (別の `--agent` は断られる)。

## トラブルシュート (実機で出たもの)

1. **「スクリプト (先頭が #!) です」と断られる (古い goro では、終了コード 127 と `No such file or directory`)**: PATH のエージェントがラッパースクリプト (Arch の claude など)。檻には、スクリプトだけが見え、それが呼ぶ実体が見えない。実体を `--bin PATH` か環境変数 `GORO_<名前の大文字>` (例: `GORO_CLAUDE`) で指す。
2. **ログインした直後なのに、次の起動で、またログインを求められる (claude)**: `claude auth login` は、onboarding の完了を保存しない。そのため、`--login` は素の対話起動にしてある。古い状態から始めた人 (旧レイアウトは `<state>/home`) は、エージェント専用 HOME を消してやり直す (消すのは利用者。こちらで消さない)。
3. **終了後の「拒否された宛先」**: 既定の許可 (エージェントごと。`goro run -h`) 以外は拒否される。説明つきで出るもの (opencode の `registry.npmjs.org`・`models.opencode.ai`・ripgrep のための `github.com`。正は `cmd/goro/agent.go` の `denyNotes`) は、許可しなくてよい。説明が無い宛先は、それが無いと起動・ログイン・1 往復ができないと分かってから、`--allow HOST:PORT` で足す (足す前に、その宛先が何かを確かめる)。古い goro では、claude でも `downloads.claude.ai`・`github.com` が出る (今は環境変数で止めている。goro を更新する)。
4. **「TIOCSTI が有効」で起動しない**: `/proc/sys/dev/tty/legacy_tiocsti` が 0 でない。0 にする (`sysctl dev.tty.legacy_tiocsti=0`) のは、利用者。
5. **opencode の grep ツールが失敗する**: 檻の中に `rg` が無く、download しようとして `github.com` が拒否される。ホストに ripgrep を入れる。`github.com` は許可しない。
6. **「egress の UDS の path が長すぎる (… 上限 107)」**: `--state-dir` を短い path にする。
7. **`--claude` は廃止**: `--bin PATH` を使う (環境変数 `GORO_CLAUDE` などは、そのまま使える)。

## 安全上の注意 (限界。正は `cmd/goro/doc.go` の「限界」)

- エージェント専用の HOME は、そのエージェントの**全セッションで共有**する。同時に動く別セッションの檻は、共有の HOME の UDS などで通信できるので、`--allow` はセッションごとの境界ではない。
- 許可した宛先 (`api.anthropic.com` など) 経由の持ち出しは防げない (TLS の中身は見ない)。
- 端末に直結するので、檻が端末にエスケープシーケンスを書ける (pty のフィルタは未実装)。信頼できない出力は、そのまま端末に出る前提で扱う。
- opencode は、repo の設定 (`opencode.json`・`.opencode/`) でコマンドを動かせる (動くのは檻の中)。信頼できない repo は、その設定が檻の中で動く前提で扱う。

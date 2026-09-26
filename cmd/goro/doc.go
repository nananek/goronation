// Command goro は goronation の CLI で、信頼できない AI コーディングエージェント (claude・opencode) を、ホストから隔てた檻の中で動かす。
//
// run は、ホストの repo の private clone (または前のセッション) の上で、ネットワークの無い bwrap の檻の中のエージェント (--agent claude か opencode。既定は claude) を動かし、
// 檻の外向き通信を、ホストの egress (許可した宛先だけの CONNECT プロキシ) だけに絞る。export は、clone のコミットを bundle にして取り出す。
// エージェントごとに違うのは、実行ファイルの解決 (--bin、GORO_<名前>、PATH。スクリプトは断る)・環境変数・既定の許可宛先・--login の起動・状態のディレクトリ (すべて <state>/agents/<名前>/{home,login-work,login-run}。どのエージェントも同じ形) だけ。セッションは、作ったエージェントを記録し (goro sessions に出る)、--session はそのエージェントで動かす (別の --agent は断る: clone に残る設定を、別の檻で動かさない)。
// sessions は一覧を出し、init は、檻の中で最初に動くリレーで、run が起動する。オプションは goro <サブコマンド> -h に書く。
//
// # 使い方
//
//	goro run --login             # 初回: claude を対話起動する。ログインし (URL をホストのブラウザで開き、出たコードを貼る)、Security notes で Enter を押したら /exit
//	goro run --repo ~/work/foo   # foo の private clone の中で claude と対話する (再開は --session ID。opencode は、--agent opencode を足す。初回は --agent opencode --login)
//	goro export ID               # bundle を作り、取り込みの git fetch を表示する (自分の repo で実行する)
//
// # 規則
//
//   - cage: 檻に入るのは、/usr・証明書・エージェントと goro の実体・run dir (すべて ro)、檻専用の HOME と clone (rw)、許可リストの環境変数だけ。ホストの HOME・~/.ssh・~/.claude・~/.local/share/opencode・環境変数は見えない。
//   - egress: 許可は、エージェントごとの既定の宛先 (goro run -h に出る) に、--allow で足したもの。拒否は、終了後に宛先つきで表示する。監査 (run dir の egress.log) は、詰まっても止まらず、行を捨てて数える。
//   - signal: 端末のシグナルは、檻の中のエージェントが直接受ける (goro init は転送しない)。ホストの goro run は SIGINT・SIGQUIT を無視し、SIGTERM・SIGHUP で檻を止める。
//   - no-host-git: ホストは git を実行しない。clone も export も使い捨ての檻の中で行い、bundle の取り込みは、利用者が自分の repo で行う。
//
// # 限界
//
//   - 許可した宛先 (api.anthropic.com・opencode.ai など) 経由の持ち出しは防げない (TLS の中身を見ない)。大量の拒否 CONNECT で監査の予算 (8 MiB) を使い切られると、以降の宛先は egress.log に載らない (捨てた行数は終了時に表示する)。
//   - 檻の HOME は、エージェントごとに、全セッションで共有する (ログイン状態・会話の履歴を残すため)。同時に動く別セッションの檻は、共有の /home/goro の UDS などで通信でき、--allow はセッションごとの境界ではない。
//   - 端末に直結するため、檻が端末に任意のエスケープシーケンスを書ける (pty の中継とフィルタは未実装。TIOCSTI は legacy_tiocsti の確認で塞ぐ)。termios は、終了後に戻す。
//   - 起動後の Ctrl-C は、エージェントの中断として効く (goro 自身は終了しない。終了はエージェントの終了操作か SIGTERM)。seccomp・cap-drop は未実装。repo は、ローカルの path だけ。Linux (bwrap) だけ。
//
// # 関連
//
// docs/adr/0001-repository-layout.md
package main

// Command goronation は goronation の CLI で、信頼できない AI コーディングエージェント (claude・opencode) を、ホストから隔てた檻の中で動かす。
//
// run は、ホストの repo の private clone (または前のセッション) の上で、ネットワークの無い bwrap の檻の中のエージェント (--agent claude か opencode。既定は claude) を動かし、
// 檻の外向き通信を、ホストの egress (許可した宛先だけの CONNECT プロキシ) だけに絞る。export は、clone のコミットを bundle にして取り出す。
// エージェントごとに違うのは、実行ファイルの解決 (--bin、GORONATION_<名前>、PATH。スクリプトは断る)・環境変数・既定の許可宛先・--login の起動・認証情報の共有と、HOME の種・状態のディレクトリ (すべて <state>/agents/<名前>/{auth,homes,login-home,login-work,login-run}。どのエージェントも同じ形) だけ。HOME は repo ごと (homes/<repo のキー>) で、認証情報だけ、エージェントごとの auth/ を全 repo の檻に渡す。セッションは、作ったエージェントと repo のキーを記録し (goronation sessions に出る)、--session はそのエージェント・その HOME で動かす (別の --agent は断る: clone に残る設定を、別の檻で動かさない)。 sessions は一覧を出す。auth は、資格情報 (github のトークン) を、ホストのファイル <state>/credentials/<名前> (0600) に保存する (値は表示せず、檻には入れない)。run --push owner/repo は、git push・fetch と PR 作成を、その 1 repo だけに許す。pr create は、檻の中から PR を作る (常に draft)。pr ready は、ホストから checks を確かめて ready にする。init は、檻の中で最初に動くリレーで、run が起動する。web は、WebAuthn (passkey) でログインしたブラウザから操作できる常駐サーバーで、セッション一覧・repo の選択・端末ビュー (xterm.js) の画面を持つ。実際の檻は、web が (必要なら子プロセスとして起こす) serve が保持し、web は認証済みの WebSocket 接続を、serve の UDS へ中身を解釈せず中継する。serve --chat は、端末ビューの代わりに、stream-json を檻で回す構造化チャット (claude だけ。非 root だけ) を、UDS chat.sock の SSE (GET /events) と、POST /message・/permission (hello の世代が必須)・/stop で出す (ADR 0013)。オプションは goronation <サブコマンド> -h に書く。
//
// # 使い方
//
//	goronation run --login             # 初回: 既定のエージェント (claude) のログイン。エージェント自身の画面の指示に従い、終わったら終了する
//	goronation run --repo ~/work/foo   # foo の private clone の中で claude と対話する (再開は --session ID。opencode は、--agent opencode を足す。初回は --agent opencode --login)
//	goronation export ID               # bundle を作り、取り込みの git fetch を表示する (自分の repo で実行する)
//
// # 規則
//
//   - cage: 檻に入るのは、/usr・証明書・エージェントと goronation の実体・run dir (すべて ro)、repo ごとの HOME・認証用ディレクトリ (auth/)・clone (rw)、許可リストの環境変数だけ。別の repo の HOME・ホストの HOME・~/.ssh・~/.claude・~/.local/share/opencode・環境変数は見えない。
//   - egress: 許可は、エージェントごとの既定の宛先 (goronation run -h に出る) に、--allow で足したもの。拒否は、終了後に宛先つきで表示する。監査 (run dir の egress.log) は、詰まっても止まらず、行を捨てて数える。
//   - signal: 端末のシグナルは、檻の中のエージェントが直接受ける (goronation init は転送しない)。ホストの goronation run は SIGINT・SIGQUIT を無視し、SIGTERM・SIGHUP で檻を止める。no-host-git: ホストは git を実行しない (clone・export も、使い捨ての檻の中)。
//   - push: --push owner/repo は、その 1 repo・セッションの ref 名前空間 (refs/heads/goronation/<セッション>/) だけに、
//     git push・fetch・PR 作成を限る (トークンは檻に渡さない。goronation pr ready は gh を使わず GraphQL を直接叩く)。
//
// # 限界
//
//   - 許可した宛先 (api.anthropic.com・opencode.ai など) 経由の持ち出しは防げない (TLS の中身を見ない)。大量の拒否 CONNECT で監査の予算 (8 MiB) を使い切られると、以降の宛先は egress.log に載らない (捨てた行数は終了時に表示する)。
//   - 檻の HOME は、repo ごとに、同じ repo の全セッションで共有する (履歴・メモリが続く)。同じ repo の別セッションの檻は、共有の /home/goronation の UDS などで通信でき、--allow は同じ repo のセッション間の境界ではない。認証情報 (auth/) は、そのエージェントの全 repo の檻から読み書きでき (信頼できない repo の檻も)、許可した宛先経由で持ち出せ、別のアカウントに差し替えられる。信頼できない repo の檻が auth/ に置いた symlink は、別の repo のセッションが認証情報を書くときに辿られ、その檻の自分のファイル (HOME・clone) を壊せる (opencode の symlink 方式。claude は rename で書くので当たらない。破壊のみで、読みも内容の指定もできない)。repo のキーは実 path で、repo の実体ではない: 同じ path に別の repo を置くと、履歴・メモリ・trust を引き継ぐ。
//   - 標準入出力の 3 つとも実端末なら、goronation run 専用の pty を用意し、そこだけに中継する (エスケープシーケンスは解釈もフィルタもしない。TIOCSTI は、専用の pty にしか効かず、legacy_tiocsti に頼らない)。3 つのどれかが端末でなければ (redirect・pipe)、これまでどおりホストの標準入出力に直結する (TIOCSTI は legacy_tiocsti の確認で塞ぐ)。termios は、終了後に戻す。
//   - 起動後の Ctrl-C は、エージェントの中断として効く (goronation 自身は終了しない。終了はエージェントの終了操作か SIGTERM)。seccomp・cap-drop は未実装。repo は、ローカルの path だけ。Linux (bwrap) だけ。檻からホストの localhost には届かず、ローカルのモデルサーバー (Ollama・LM Studio など) は使えない。goronation web は、同時接続数の上限 (64) を keep-alive 無効化で構造的に守るが、body をゆっくり送って都度接続を張り直す変種の可用性 DoS までは防がない (ログイン前の endpoint に届く主体は、無認証でも一時的に service を止められる)。goronation serve は UDS 専用で、HTTP の認証 (WebAuthn・cookie) を持たない: UDS に繋げること自体を信頼の境界にし、繋いできた相手を無条件に信頼する (ファイルシステムの権限 (置き場所のディレクトリが 0700・ソケット自体が 0600) が、実際の防御線)。goronation web と goronation serve は別プロセス・別ライフサイクルで、goronation web は起こした goronation serve の生存を追跡しない (プロセスが落ちても、次に dial するときに自動で起こし直す)。
//
// # 関連
//
// docs/adr/0001-repository-layout.md
package main

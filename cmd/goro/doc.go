// Command goro は goronation の CLI で、`goro <サブコマンド>` を振り分ける。
//
// いまのサブコマンドは init だけである。init は、檻 (ネットワークが無い bwrap) の中で最初に動く小さなリレーで、
// 檻の loopback の TCP を、ホストの egress プロキシの Unix ドメインソケット (UDS) へ中継しながら、子プロセスを起動する。
// 子の stdio (端末) と環境変数を引き継ぎ、子が終われば、子の終了コードで終わる。子の出力は解釈せず、リレーは何も書かない
// (init が stderr に書くのは、引数の不正と、子を起動できなかったときだけ)。
//
// # 使い方
//
//	goro init --listen 127.0.0.1:0 --upstream /run/goro/egress.sock -- claude
//
// # 規則
//
//   - relay: 接続ごとに上流の UDS へ繋いで双方向に中継する。片方が EOF なら、その向きの書き側だけを閉じる (半クローズ)。
//     読み書きの失敗は、両方を閉じる。上流に繋がらない接続と、同時 128 を超える接続は、黙って閉じる。
//   - listen: --listen は loopback の IP リテラルとポート (0 は空きポート) だけを受け付ける。
//   - proxy-env: 子に HTTPS_PROXY・HTTP_PROXY・https_proxy・http_proxy を、実際の待ち受け先 (ポート 0 なら、決まったポート) を
//     指す http の URL にして渡す。NO_PROXY は設定しない。--no-proxy-env で、この設定をやめる。
//   - signal: SIGINT・SIGTERM・SIGHUP・SIGQUIT・SIGWINCH を子に転送する。
//   - reap: 自分が PID 1 のときは、孤児を回収する (wait4(-1))。
//   - exit-code: 子の終了コード (シグナルで死んだら 128+番号)。引数が不正なら、使い方を stderr に出して 2。
//     子を起動する前の失敗は 125、実行できないは 126、見つからないは 127。
//
// # 限界
//
// init は、檻を作らず、自分が檻の中にいることも確かめない。ホストで動かしても、同じように動く。
// リレーは宛先を見ずにバイト列を通す。許可先の判定は、上流のホスト側の egress が行う。
// proxy の環境変数を尊重しない子は、外へ届かない (檻にネットワークが無いため。init は、そのような子を検出しない)。
package main

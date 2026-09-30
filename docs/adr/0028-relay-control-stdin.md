# 0028. 要求の中継の control は init の標準入力にし、HTTP の解釈と上限を固定する (M2)

- 状態: 採用 (2026-09-30 に追記: 横取りの「PR③ で再検討」は ADR 0029、ヘッダの許可リストは ADR 0030 が引き継いだ)
- 日付: 2026-09-30
- 関連: [Issue #1](https://github.com/nananek/goronation/issues/1)、ADR 0012・0019・0020、`cmd/goronation/relay_http.go`・`relay_control_linux.go`・`init_relay_linux.go`

## 状況

ADR 0019 は、要求ごとの socketpair を `SCM_RIGHTS` で init に送る、と決めた。送る経路 (control) を檻の中に渡す必要がある。`sandbox/bwrap.Spec` は、標準入出力の 3 つしか fd を渡せない (fd を足すと、契約 `sandbox/contract` の変更になる)。chat セッションの init の標準入力・標準出力は、すでにホストとの別々の socketpair (ADR 0012) で、init は PID 1・dumpable=0 で守られている。

## 決定

1. **control は、init の標準入力 (fd 0) の socketpair にする。** `Spec` は変えない。init は `--relay-control --relay-port N` (要 `--non-dumpable`。ないと起動しない) で、fd 0 を control にして、子に渡さない。子 (opencode) の標準入出力は、init が別に作る pipe (標準入力は init が書き側を持ち、init が死ぬと閉じる。標準出力は、PR③ が `{"url":…}` の確認に読む)。control が閉じられたら (会話の終了・ホストの終了)、init は子に SIGTERM を送り、5 秒たっても終わらなければ SIGKILL を送る (SIGTERM を無視する子で檻が終わらないのを避ける)。
2. **control の規約**: 1 バイトのデータと、`SCM_RIGHTS` の fd 1 個を 1 メッセージにする。init は 1 バイトずつ受ける。受信は `MSG_CMSG_CLOEXEC`。fd が 0 個・2 個以上・oob の上限 (4 個) 超え・socket でない・stream でない・unix domain でないものは、受けた fd を全部閉じて、読みを続ける。
3. **HTTP の解釈は最小。** 通すのは、メソッド GET・POST・PUT・PATCH・DELETE、HTTP/1.1、target は `/api/` で始まる origin-form だけ (`.`・`..` の区間と、エンコードされた `.`・`/`・`\` は、上流の正規化で `/api/` の外へ出るので拒否)。`Authorization`・`Proxy-Authorization`・`Host`・`Connection`・経路のヘッダ (`Forwarded`・`X-Forwarded-` で始まる名前すべて・`X-Real-IP` など) は、大文字小文字・重複・空白に関わらず捨て、init が `Host` (127.0.0.1:ポート)・`Authorization` (トークン)・`Connection: close` を 1 つずつ付ける。`Transfer-Encoding`・`Upgrade`・`Expect`・行の折り返し・LF だけの行・非 ASCII・重複や不正な `Content-Length` は拒否する。本文は `Content-Length` ちょうど分だけ流し、後に続くバイト (pipelining) は上流に届けない。
4. **上限**: 先頭 32 KiB・1 行 16 KiB・ヘッダ 64 個・本文 1 MiB。先頭は 10 秒・本文は無通信 10 秒・ホストへの書き込みは 30 秒・上流の無通信は 2 分で切る。同時数は、全体 64・短い要求 32・SSE (`GET /api/event`) 8 (SSE が枠を使い切っても、承認の返答が通る)。枠を超えた fd は、何も読まずに閉じる (検査後の超過は 503)。
5. **応答は解釈しない。** バイトのまま、バッファせずに返す (SSE)。ホストが接続を閉じたら上流も閉じる (ホストは、要求のあとに書き側だけを閉じず、全部閉じる)。
6. **接続先は init が決めた `127.0.0.1:<--relay-port>` だけ。** トークンの供給元 (`func() string`) は差し込める形で、生成と opencode への受け渡しは PR③ が持つ。

## 帰結

- control が乗っ取れるかは、init の dumpable=0 (ADR 0012) に依存する。CI の runner は ptrace_scope=1 で、変異で確かめる。
- 標準入力が control になるので、chat セッションの `Input` (エージェントの入力) は、control とは別の経路 (transport。ADR 0022) になる。
- **子の死と待ち受けの横取り (既知の限界・PR③ で必須で再検討)**: init は TCP の loopback (`127.0.0.1:<ポート>`) に、トークンつきで接続する。opencode が SIGKILL されたあと、同じ uid の別プロセスが同じポートを bind し直すと、その間に届いた要求 (トークンを含む) を受け取れる (攻撃者視点レビューで再現: 緩和前は 25 回中 6 回、緩和後も約 10.5%)。この PR の緩和は、init が子の死を回収した直後に要求の受け付けを止める (control を閉じ、検査中・接続前の要求も上流に繋がない) ことだけで、窓を狭めるだけで消さない。この緩和は「子の死」だけを引き金にするので、危険の本質 (待ち受けポートの持ち主が想定した上流でないこと) の一部にしか対応しない: 子が生きたまま、別のプロセスが先にポートを bind していれば (opencode の再起動・起動順など)、子の死を待つ緩和は発動せず、要求とトークンがそのプロセスに渡る (攻撃者視点レビューで再現)。**PR③ は、トークンの設計のときに、UDS 化・init が bind したソケットの継承 (fd) など、TCP の待ち受けの横取りが原理的に効かない形を必ず再検討する。トークンが使い捨てでなく再利用されるなら (opencode の再起動で使い回す・檻の外と共有する)、Blocking に上がる。**
- **拒否リスト方式の限界 (PR③ で許可リストを検討)**: 要求のヘッダは、捨てる名前を列挙する方式で、`Content_Length` などアンダースコア版・`X-Original-URL`・`X-Http-Method-Override` などは上流に届く。上流 (opencode) がアンダースコアをハイフンと同一視すると、要求の密輸になりうる (未検証)。送り手はホストだけだが、PR③ でホストが値を組み立てるときに、通すヘッダの許可リスト方式への変更を検討する。

## 代替案

- `Spec` に fd を足す (案 B): 契約の変更で、レビューの対象が増える。標準入力で足りる。
- path の UDS: ADR 0019。
- `net/http` のサーバー・`httputil.ReverseProxy`: 既定の緩い上限と解釈の広さが、PID 1 の攻撃面になる。

// Package egress は、檻の外向き通信を、許可した宛先 (host:port) だけに絞る CONNECT プロキシ。
//
// 檻はネットワークを持たず、ホストの Unix ドメインソケットだけでこれに届く。TLS の中身は見ず、資格情報も注入しない。
// 許可した宛先への中継と、内部の IP へ接続しないことを保証する。許可した宛先の先で何が送られるかは、保証しない。
//
// # 使い方
//
//	s := egress.New(egress.Config{Allow: egress.ClaudeHosts(), Audit: os.Stderr})
//	err := s.Serve(l) // l は caller が開いた listener。Close か、l の終了で返る
//
// # 規則
//
//   - allow-exact: 宛先は Allow との完全一致。host は大文字小文字を区別せず、ワイルドカードは無い。
//   - connect-only: HTTP/1.x の CONNECT だけを受ける。他のメソッド・userinfo・IP リテラル (127.1 のような省略形も) は 4xx で拒否する。
//   - dial-check: 接続する IP を、dialer の Control で検査する。loopback・private・CGNAT・link-local・multicast・unspecified (IPv4-mapped も) は拒否する。
//   - limits: 同時接続数・ヘッダの大きさと期限・dial の期限・アイドルの期限に上限がある (Config)。
//   - audit: 許可・拒否・接続の失敗を、理由つきで 1 行の JSON にして出す。秘密は出さず、監査に書けない許可は出さない。
//
// # 限界
//
//   - IPv4 を埋め込む IPv6 (NAT64・6to4) と、0.0.0.0/8・予約の IPv4 は、禁止しない (TestForbiddenLimits)。
//   - 許可した宛先を経由した持ち出しは、防げない。TLS の中身を見ないため。
//   - 名前の解決はホストの resolver に頼る。許可した名前が公開 IP に解決されれば、それが偽でも接続する。
package egress

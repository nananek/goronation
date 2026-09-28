// Package connectproxy は、cmd/framecapture 専用の、SSRF 対策を持たないテスト専用の最小限の CONNECT-only HTTP プロキシ。
//
// 本番の egress.Server ではない (internal に置き、cmd/framecapture の外からは import できない)。
// egress.Server とは違い、汎用の egress 制御ではない: 起動時に渡した小さな固定の対応表
// (クライアントが CONNECT 行に書く仮想の "host:port" → 実際に dial する実アドレス) にある宛先だけを
// 許可する。egress.Server の forbidden-IP 判定 (loopback・private・CGNAT などへの接続を拒む SSRF 対策)
// は、意図的に持たない: 対応表にある実アドレスを無条件で信頼して dial する。
//
// 想定する使い方は、cmd/framecapture が檻の中で動かす claude/opencode から、同じ檻の外 (ホストの
// loopback) で自分自身が起動した fake provider サーバーへ、CONNECT (+ TLS) で到達させることだけ。
// 本物の資格情報を扱う経路や、実際のネットワークに触れる経路には、絶対に使わない (それは egress.Server
// の役目)。
//
// # 使い方
//
//	p := connectproxy.New(map[string]netip.AddrPort{
//		"fake-anthropic.test:443": netip.MustParseAddrPort("127.0.0.1:38421"),
//	})
//	err := p.Serve(l) // l は呼び手が開いた listener。Close か、l 自身の Close で戻る
package connectproxy

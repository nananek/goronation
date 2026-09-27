package egress

import "net/netip"

// ForbiddenAddr は、egress の CONNECT が上流への接続に使う、接続してはいけない IP の判定 (loopback・private・CGNAT・
// link-local・multicast・unspecified) を、egress/** の他の中継 (git smart-HTTP・PR 作成のエンドポイント) に公開する。
// 判定そのものは forbidden と同じ (dial-check を、そこでも再利用するため)。
func ForbiddenAddr(a netip.Addr) bool { return forbidden(a) }

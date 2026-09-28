// Package gateway は、egress の git smart-HTTP と PR 作成のエンドポイントで、檻の git・goronation pr create の要求を、
// egress/git・egress/github の検査に通してから、ホストの資格情報を付けて GitHub へ中継する http.Handler を提供する。
//
// 経路・repo・本文の検査 (git.Policy.AuthorizedRoute・github.ParsePull) を必ず通した Route・Pull だけを中継する。
// 中継の実務 (TLS・ヘッダの取捨・リダイレクトの拒否・大きさと時間の上限・監査) は、この package が担う。
//
// # 使い方
//
//	h, err := gateway.New(gateway.Config{Push: push, Pull: pull, Credentials: src})
//	err = egress.ServeBoth(l, connect, h, git.PathPrefix, github.PathPrefix)
//
// # 規則
//
//   - single-entry: git は Policy.AuthorizedRoute、PR は PullPolicy.ParsePull だけを経路の入口にする (呼び忘れを作らない)。
//   - rebuild-url: 上流の URL は、検査した Route・Pull と、設定の base URL から作る。要求の生の path は使わない。
//   - forward-raw: receive-pack は、検査したコマンド部と、続きのバイト列を、そのまま上流に送る。
//   - header-allowlist: 上流へ送るヘッダは、Content-Type・Accept・Git-Protocol・固定の User-Agent だけ。応答も allowlist。
//   - no-redirect・dial-check: 上流への接続は、リダイレクトを追わず、egress.ForbiddenAddr で禁止 IP を断る。
//   - size: pack は Config.MaxPackBytes (既定 512 MiB) を http.MaxBytesReader で強制。PR の要求・応答にも上限。
//   - token: Authorization は、中継の直前に Credentials.Token で取り、値は応答・error・監査に出さない。
//   - quota: PR の作成は、要求ごとに Quota.Take を通す。
//   - no-keep-alive: git/PR 経路は 1 接続 1 要求 (keep-alive を切る)。次の要求は必ず新しい接続・
//     connect.sem の枠を要するため、絶対締め切り・MaxConns を、要求の繋ぎ直しで回避できない。
//
// # 限界
//
//   - Authorization の形式 (git は Basic の x-access-token・API は Bearer) を、GitHub の実機で確かめていない。
//   - 読む側の締め切り (ヘッダ・本文の絶対締め切り・同時接続数) は、この層が持つ (MaxBodyBytes は、
//     呼び手が MaxPackBytes と揃えること・自動では同期しない)。書く側・upload-pack (fetch) の本文検査は、まだ無い。
//   - 1 つの listener での CONNECT との共存は egress.ServeBoth が行うが、goronation run への配線は、この package の外 (PR ③)。
package gateway

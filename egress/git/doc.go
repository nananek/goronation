// Package git は、檻の git の smart-HTTP の要求を、許可した repo と ref だけに絞る検査で、ネットワークに触れない純関数の集まり。
//
// 檻の git は、信頼できない。要求の経路と、receive-pack (push) の本文のコマンド部を、厳格な文法で検査し、通ったものだけを上流へ流す部品になる。
// 保証するのは、検査を通した要求が、許した repo の、セッション専用の ref (refs/heads/goronation/<セッション>/…) の作成・更新だけであること。
// 上流の応答・pack の中身・force push は、見ない。
//
// # 使い方
//
//	route, err := git.ParseRoute(method, target) // 経路。repo は p.CheckRepo で確かめる
//	push, err := p.ReadPush(body, git.Limits{})  // OpReceivePack の本文。上流へは push.Raw と、body の残りを続ける
//
// # 規則
//
//   - route: GET info/refs?service=git-{upload,receive}-pack と、POST git-{upload,receive}-pack の 4 つだけ。%・..・//・余計な query は断る。上流の path は、検査した値から作る。
//   - packet: pkt-line の長さは小文字の 16 進 4 桁。0001〜0004 と 65520 超は断る。コマンド部は 16 KiB・32 件まで。読む前に上限を見る。
//   - command: 行は "old new ref"。oid は 40 桁の小文字の 16 進 (SHA-256 は断る)。ref は印字できる ASCII。NUL は先頭の行に 1 回だけ。
//   - capability: report-status(-v2)・side-band-64k・quiet・atomic・ofs-delta・object-format=sha1・agent= だけ。重複・未知のものは断る。
//   - refuse: shallow・push-cert・push-options・削除 (new が 0)・許可していない名前空間の ref は、1 件でもあれば全体を断る。
//   - forward-raw: 通したコマンド部は、読んだバイト列 (Raw) のまま流す。解釈し直した値を送らない。
//   - probe: コマンドが 0 件の本文 (0000 だけ。git が大きい push の前に送る) は、続きが無いときだけ通す。
//
// # 限界
//
//   - force push は、検出できない (プロトコルに旗が無く、fast-forward の判定は pack と上流の ref が要る)。害は、セッション専用の名前空間で限る。
//   - pack の中身は見ない。大きさの上限と、時間の上限は、呼び手の責任 (コマンド部の後を読まない。例外は探りで、続きが無いことを確かめる 1 バイトを読む。それも締め切りの対象)。
//   - upload-pack の本文 (fetch) は、検査しない。読めるのは、許可した repo の全体。
//   - SHA-256 の repo は扱わない (TestParseCommandsCaptured の sha256)。
//   - 上流は github.com だけ (PathPrefix)。GitHub Enterprise・SSH・dumb HTTP は扱わない。
package git

// Package contract は、サンドボックス契約 (core/sandbox) の共通検証で、機密 path・環境変数名・継承 fd を、バックエンドによらず同じ規則で断る部品を提供する。
//
// バックエンドは、翻訳の前に Rules.Validate を、起動の前に Rules.Resolve を通し、CloseOnExecFrom で、継承した fd を檻に渡さない。
// 規則は文字列と、symlink を辿った実体の path だけを見る (検証の後の差し替えは見ない)。値の中身は見ない。
//
// # 使い方
//
//	r := contract.Rules{Host: contract.CurrentHost(), Policy: contract.LinuxPolicy(), Caps: caps}
//	err := r.Validate(spec) // errors.Is(err, sandbox.ErrRejected)
//
// # 規則
//
//   - secret-path: ホストの HOME 自体・機密の path (~/.ssh・~/.claude など)・システムの機密 (/root・/run・/etc/ssh など)・認証用 socket は、Read にも Write にも入れない。
//   - in-home: HOME の下の HostPath は、InHome を明示した作業用 dir だけ。付けずに HOME の下を指す・付けたのに HOME の外を指すと断る。
//   - resolved: Resolve は、symlink を辿った実体にも、同じ規則をかける (機密を指す symlink を、別の名前で見せない)。
//   - env-allowlist: 環境変数の名前は英数字と _ だけ。資格情報らしい名前 (SSH_AUTH_SOCK・*_TOKEN・AWS_* など) と、重複は断る。
//   - path-form: 絶対・クリーンで、制御文字を含まない path だけ。GuestPath が HostPath と違うのは、PathRemap のバックエンドだけ。
//   - egress-dir: Egress の親ディレクトリは、Read で見せ、Write では見せない。Loopback は、loopback の IP リテラル:ポートだけ。
//   - inherited-fd: CloseOnExecFrom は、継承した fd (3 以降) を close-on-exec にする。できなければ error。
//
// # 限界
//
//   - システムの機密 path の表 (Policy) は、OS ごとで、LinuxPolicy だけがある。macOS の表は、seatbelt の実装が足す。
//   - 検証の後の差し替え (symlink の付け替え) は見ない。値の中身 (環境変数の値に含まれる秘密) は見ない。
//   - CloseOnExecFrom は、呼び手のプロセス全体に効く。unix 以外は、対応しない。
//
// # 関連
//
// docs/adr/0001-repository-layout.md
package contract

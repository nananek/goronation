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
//   - resolved: Resolve は、symlink を辿った実体にも、同じ規則をかけ、実体の path を返す (Spec は書き換えない)。起動器に、字面でなく実体を渡すのは、バックエンドの責任。
//   - env-allowlist: 環境変数の名前は英数字と _ だけ。資格情報らしい名前 (SSH_AUTH_SOCK・*_TOKEN・AWS_* など) と、重複は断る。宣言された ExtraEnv の名前にも、同じ規則をかける。
//   - path-form: 絶対・クリーンで、制御文字を含まない path だけ。GuestPath が HostPath と違うのは、PathRemap のバックエンドだけ。
//   - guest-path: 檻の中の path は、/・バックエンドが常に作る path (Linux は /proc・/dev) の中・System が占める path (/usr と symlink) と重ならない。Scratch は、Mount・基盤の内側に置けない (後から bind される Mount が隠す)。
//   - egress-dir: Egress の親ディレクトリは、Read で見せる (同じ GuestPath の Write は、重複で断る)。Loopback は、loopback の IP リテラル:ポートだけ。
//   - inherited-fd: CloseOnExecFrom は、継承した fd (3 以降) を close-on-exec にする。できなければ error。
//
// # 限界
//
//   - システムの機密 path の表 (Policy) は、OS ごとで、LinuxPolicy だけがある。macOS の表は、seatbelt の実装が足す。
//   - 検証の後の差し替え (symlink の付け替え) は見ない。値の中身 (環境変数の値に含まれる秘密) は見ない。環境変数名の表は目安で、網羅ではない。
//   - 大文字小文字を区別しない FS・Unicode の正規化 (NFC/NFD) の別綴りは、正規化しない (/Users/x と /users/x は、別の path)。macOS のバックエンドは、Host に区別の有無を持たせるなど、実装側で対処する (API は、seatbelt が入る時点で決める)。
//   - CloseOnExecFrom は、呼び手のプロセス全体に効く。unix 以外は、対応しない。
package contract

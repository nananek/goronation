package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// Backend は、サンドボックスのバックエンド。名前をキーにした表 (cmd が持つ) で選ぶ。
type Backend interface {
	// Name は、バックエンドの名前 ("bwrap"・"seatbelt")。
	Name() string
	// Probe は、起動器が在り、使えるかを確かめる。無ければ ErrNotInstalled、在っても使えなければ ErrUnusable を包んだ error を返す。
	Probe(ctx context.Context) error
	// Capabilities は、バックエンドが満たす任意項目の宣言。
	Capabilities() Capabilities
	// Prepare は、s を検証し、起動器の引数に翻訳する。何も起動せず、ファイルシステムも見ない。
	// 検証に通らなければ、ErrRejected を包んだ error を返す。
	Prepare(s Spec) (Prepared, error)
	// Start は、s を検証し (symlink を辿った実体も)、檻を起動する。契約を満たせると確かめられなければ、起動しない。
	Start(ctx context.Context, s Spec) (Cage, error)
}

// Cage は、起動した檻 1 つ。
type Cage interface {
	// Wait は、檻のコマンドの終了を待つ。0 で終われば nil、そうでなければ *ExitError を返す (シグナルで死んだら、終了コードは 128 + 番号)。
	Wait() error
	// Signal は、檻のコマンドを止めるために、シグナルを送る。
	Signal(sig os.Signal) error
}

// Prepared は、Spec をバックエンドの起動器の引数に翻訳したもの。起動はしない。
type Prepared struct {
	// Argv は、起動器の argv (argv[0] は、固定パスの起動器)。翻訳の結果を、そのまま比べる (golden) ためのもの。
	Argv []string
}

// Capabilities は、バックエンドが宣言する、任意の項目。true は「満たす」、false は「満たさない」で、適合テストが、どちらも確かめる。
type Capabilities struct {
	// PathRemap は、檻の中の path を、ホストの path と別にできるか (bwrap の mount の付け替え)。false なら、GuestPath = HostPath だけ。
	PathRemap bool
	// PrivateLoopback は、檻の loopback が、ホストと別か (別なら、檻の待ち受けに、ホストのプロセスは届かない)。
	PrivateLoopback bool
	// KillsDescendants は、檻のコマンドが終わったとき、その子孫 (setsid したものを含む) が、全員終わるか。
	KillsDescendants bool
	// PrivatePIDs は、檻から、ホストのプロセスが見えないか。
	PrivatePIDs bool
	// ExtraEnv は、Spec.Env に無くても、バックエンドが檻に足す環境変数の名前 (bwrap は PWD)。資格情報らしい名前は、宣言できない (契約の検証が断る)。
	// ホストの環境変数の値を、檻に渡してはならない (宣言の有無に依らない。適合テストが、ホストに置いた値が檻に漏れたら、不合格にする)。
	ExtraEnv []string
}

// Cap は、Capabilities の項目の名前。適合テストが、項目ごとに、宣言と実際を比べるための識別子。
type Cap string

// Capabilities の項目の名前。
const (
	CapPathRemap        Cap = "path-remap"
	CapPrivateLoopback  Cap = "private-loopback"
	CapKillsDescendants Cap = "kills-descendants"
	CapPrivatePIDs      Cap = "private-pids"
)

// Has は、c が、項目 name を満たすと宣言しているか。未知の名前は false。
func (c Capabilities) Has(name Cap) bool {
	switch name {
	case CapPathRemap:
		return c.PathRemap
	case CapPrivateLoopback:
		return c.PrivateLoopback
	case CapKillsDescendants:
		return c.KillsDescendants
	case CapPrivatePIDs:
		return c.PrivatePIDs
	}
	return false
}

// Mount は、ホストの path を、檻に見せる 1 件。
type Mount struct {
	// HostPath は、ホストの絶対・クリーンな path。機密の path (ホストの HOME 自体・~/.ssh・~/.claude など) は、ErrRejected で断る。
	HostPath string
	// GuestPath は、檻の中の path。空なら HostPath と同じ。HostPath と違う値を持てるのは、Capabilities.PathRemap のバックエンドだけ。
	GuestPath string
	// InHome は、HostPath がホストの HOME の下にあると、呼び手が明示したもの (clone・檻専用の HOME・run dir など)。
	// HOME の下の HostPath は、これを付けたときだけ通す。付けずに HOME の下を指す・付けたのに HOME の外を指すと、断る。
	InHome bool
}

// Guest は、檻の中の path (GuestPath が空なら HostPath) を返す。
func (m Mount) Guest() string {
	if m.GuestPath != "" {
		return m.GuestPath
	}
	return m.HostPath
}

// EnvVar は、檻に渡す環境変数 1 件。
type EnvVar struct {
	Key, Value string
}

// Spec は、檻 1 つの起動仕様。ここに無いものは、檻に入らない (許可リスト方式)。
//
// Exec・Args・Dir・Scratch・Egress は、檻の中の path。Read と Write は、重なるとき (rw の中の ro など) も、内側が外側に隠れない
// (順序は、バックエンドが決める)。
type Spec struct {
	// Exec は、檻の中で実行するコマンド (絶対 path。PATH は検索しない)。Args は、その引数。
	Exec string
	Args []string
	// Dir は、檻の中の作業ディレクトリ。空なら指定しない。
	Dir string
	// Env は、檻の環境変数 (許可リスト)。ここに書いたものだけが渡る (バックエンドが足すものは、Capabilities.ExtraEnv)。
	Env []EnvVar
	// Read は、読める (実行できる) path。Write は、書ける path。
	Read, Write []Mount
	// System は、OS の基盤 (共有ライブラリ・coreutils など。bwrap では /usr と、その symlink) を、読めるようにする。
	System bool
	// Scratch は、檻専用・揮発の一時ディレクトリ (檻の中の path)。ホストに残らない。GuestPath を選ぶので、PathRemap のバックエンドだけ。
	Scratch []string
	// Egress は、檻から届く Unix ドメインソケットの、檻の中の path。空なら、届くものは無い。
	// ソケットは作り直されうるので、その親ディレクトリを、Read の Mount (GuestPath が、そのディレクトリ) で見せる。
	Egress string
	// Loopback は、檻の loopback で、待ち受けと接続をしてよいアドレス (IP リテラル:ポート)。
	Loopback []string
	// Terminal は、呼び手の端末に直結するか。true のとき、端末への注入 (TIOCSTI など) を塞げると確かめられなければ、起動しない。
	// false (既定) なら、檻は端末を持たない。
	Terminal bool
	// Stdin・Stdout・Stderr は、檻のコマンドの標準入出力。nil なら、入力は空、出力は捨てる。
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// 契約の error。呼び手は、errors.Is で判別する。
var (
	// ErrNotInstalled は、起動器が無いこと (Probe)。
	ErrNotInstalled = errors.New("sandbox: 起動器が無い")
	// ErrUnusable は、起動器は在るが、使えないこと (Probe。権限・kernel の設定など)。
	ErrUnusable = errors.New("sandbox: 起動器を使えない")
	// ErrRejected は、Spec が契約に反すること (機密の path・資格情報らしい環境変数名・不正な path など)。起動しない。
	ErrRejected = errors.New("sandbox: 起動仕様が契約に反する")
	// ErrTerminalUnsafe は、端末に直結して、注入を塞げると確かめられないこと。起動しない。
	ErrTerminalUnsafe = errors.New("sandbox: 端末への注入を塞げると確かめられない")
)

// ExitError は、檻のコマンドが 0 以外で終わったこと。シグナルで死んだら、Code は 128 + 番号。
type ExitError struct {
	Code int
}

// Error は、終了コードを含む文言を返す。
func (e *ExitError) Error() string {
	return fmt.Sprintf("sandbox: 檻のコマンドが終了コード %d で終わった", e.Code)
}

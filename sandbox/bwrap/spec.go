package bwrap

import (
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Spec は、檻 1 つの起動仕様。ここに無いものは、檻に入らない (許可リスト方式)。
//
// 檻は常に、--unshare-all (ネットワークを含む)・--die-with-parent・--clearenv・--proc /proc・--dev /dev で作る。
// これらを外す・変える手段は無く、--share-net も出せない。mount は、symlink → proc・dev → tmpfs → bind の順で作る
// (bind は Binds に並べた順。下の path の bind は、上の bind の後に置く)。
type Spec struct {
	// Host は、Argv が検証に使う、ホストの状態。Argv を純関数にするため、呼び出し側が渡す (CurrentHost が実環境から作る)。
	Host Host
	// Symlinks は、檻の中に作る symlink。
	Symlinks []Symlink
	// Tmpfs は、檻の中の tmpfs の path。
	Tmpfs []string
	// Binds は、ホストから檻へ見せる path。ホストの / を丸ごと見せる手段は無い。
	Binds []Bind
	// Env は、檻の中の環境変数 (順序つき)。ここに書いたものだけが、檻に渡る。資格情報らしい名前は error。
	Env []EnvVar
	// Chdir は、檻の中の作業ディレクトリ。空なら指定しない。
	Chdir string
	// Cmd は、檻の中で実行するコマンド。Cmd[0] は絶対 path (PATH は検索しない)。
	Cmd []string
	// NewSession は、--new-session (setsid) を付けるか。既定 (false) は付けない。付けると制御端末を失い、
	// 端末のサイズ変更 (SIGWINCH) が届かない。付けないとき、端末に直結して (標準入出力のどれかが端末か、
	// 呼び手が制御端末を持つ) 起動するなら、Start は TIOCSTI が無効なことを確かめる。
	NewSession bool
	// Stdin・Stdout・Stderr は、檻のコマンドの標準入出力。nil なら、入力は空、出力は捨てる。*os.File 以外を渡すと、
	// os/exec が pipe 越しに中継する (Stdin は、EOF になるまで、Cmd.Wait が戻らない)。
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Bind は、ホストの path を、檻の中に見せる 1 件。
type Bind struct {
	// Src は、ホストの path。絶対・クリーンで、機密の path (ホストの HOME 自体・~/.ssh・~/.claude・/run/user など) は error。
	Src string
	// Dst は、檻の中の path。絶対・クリーンで、Spec の中で重複せず、Symlinks の Dst の下にも置けない。
	Dst string
	// RW は、書き込めるか。false なら ro。システムの path (/usr・/etc など) は ro だけ。
	RW bool
	// InHome は、Src がホストの HOME の下にあると、呼び出し側が明示したもの (clone・檻専用の HOME・run dir など)。
	// HOME の下の Src は、これを付けたときだけ通す。付けずに HOME の下を指す・付けたのに HOME の外を指すと error。
	InHome bool
}

// Symlink は、檻の中に作る symlink 1 件。
type Symlink struct {
	// Target は、symlink の指す先 (相対でもよい)。
	Target string
	// Dst は、symlink を作る、檻の中の path。この下に、Binds・Tmpfs・Symlinks は置けない
	// (bwrap は symlink を辿るので、/proc を指す /p の下の /p/sys は、/proc の中に届く)。
	Dst string
}

// EnvVar は、檻に渡す環境変数 1 件。
type EnvVar struct {
	Key, Value string
}

// Host は、Spec の検証に使う、ホストの状態。
type Host struct {
	// Home は、ホストの HOME (絶対・クリーン。"/" でない)。空だと Argv は error にする。
	Home string
	// Secrets は、ホストの認証用 socket・鍵の実体 (SSH_AUTH_SOCK の値など)。これと、その配下・親は bind できない。
	Secrets []string

	// resolvedTrees は、Start が足す、拒否する path (protectedTrees・homeSecrets など) の、symlink を辿った実体。
	// 拒否する path 自体が symlink のとき (例: /etc/ssh -> /etc/.ro/ssh) に、その先を bind させないため。
	resolvedTrees []string
}

// CurrentHost は、実環境 (HOME・SSH_AUTH_SOCK・GNUPGHOME・XDG_RUNTIME_DIR・GPG_AGENT_INFO・DBUS_SESSION_BUS_ADDRESS) から
// Host を作る。HOME を決められないときは、Home が空になり、Argv が error にする。HOME 環境変数が、passwd の HOME と
// 違うときは、後者も Secrets に入れる (環境変数を差し替えて、本当の HOME を作業用 dir に見せかけさせない)。
func CurrentHost() Host {
	var h Host
	if home, err := os.UserHomeDir(); err == nil {
		h.Home = filepath.Clean(home)
	}
	add := func(p string) {
		if filepath.IsAbs(p) {
			h.Secrets = append(h.Secrets, filepath.Clean(p))
		}
	}
	if u, err := user.Current(); err == nil && filepath.Clean(u.HomeDir) != h.Home {
		add(u.HomeDir)
	}
	add(os.Getenv("SSH_AUTH_SOCK"))
	add(os.Getenv("GNUPGHOME"))
	add(os.Getenv("XDG_RUNTIME_DIR"))
	add(strings.SplitN(os.Getenv("GPG_AGENT_INFO"), ":", 2)[0])
	for _, part := range strings.Split(os.Getenv("DBUS_SESSION_BUS_ADDRESS"), ",") {
		if p, ok := strings.CutPrefix(part, "unix:path="); ok {
			add(p)
		}
	}
	return h
}

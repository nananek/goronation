//go:build unix

package hostfs

import (
	"os"
	"syscall"
)

// readFlags は、読み取りで開くときのフラグ。O_NONBLOCK を付ける: 種別の確認 (fstat) の前に、FIFO を開いても、
// writer を待って止まらない (種別は、開く前の Lstat で確かめるが、その後の差し替えでも止まらないようにする)。
// O_NOFOLLOW は付けない: os.Root は、root の中を指す symlink を自分で辿ってから open するので、付けても効かない。
// symlink を辿っていないことは、開いた fd と名前の Lstat が同じファイルであること (open の中で確かめる) で担保する。
const readFlags = os.O_RDONLY | syscall.O_NONBLOCK

//go:build unix

package main

import (
	"os"
	"syscall"
)

// linkCount は、ファイルのハードリンクの数 (同じ inode を指す名前の数) を返す。取れなければ false。
func linkCount(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

// open のフラグ。いずれも O_NONBLOCK を付ける: 種別の確認 (fstat) の前に、FIFO を開いても、
// writer (書き込みなら reader) を待って止まらない。実測: 付けないと、writer の無い FIFO の open が止まる。
// O_DIRECTORY は、ディレクトリ以外 (FIFO を含む) を開かせない。
//
// O_NOFOLLOW は付けない。実測: os.Root は、root の中を指す symlink を自分で辿ってから open するので、
// O_NOFOLLOW を付けても、symlink を辿って成功する。symlink を辿っていないことは、open した後に、
// fd と名前の Lstat が同じファイルであること (verifySame) で確かめる。
// 書き込みには O_TRUNC も付けない (その確認が済むまで、既存のファイルを触らない)。
const (
	readFlags  = os.O_RDONLY | syscall.O_NONBLOCK
	dirFlags   = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_DIRECTORY
	writeFlags = os.O_WRONLY | os.O_CREATE | syscall.O_NONBLOCK
)

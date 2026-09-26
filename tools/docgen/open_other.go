//go:build !unix

package main

import "os"

// unix 以外 (対応 OS は Linux と macOS だけ) では、FIFO で止まらないための O_NONBLOCK などが無い。
// リンク数も取れないので、ハードリンクは検出しない (linkCount は、常に「取れない」を返す)。
func linkCount(fi os.FileInfo) (uint64, bool) { return 0, false }

const (
	readFlags  = os.O_RDONLY
	dirFlags   = os.O_RDONLY
	writeFlags = os.O_WRONLY | os.O_CREATE
)

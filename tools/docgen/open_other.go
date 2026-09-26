//go:build !unix

package main

import "os"

// unix 以外 (対応 OS は Linux と macOS だけ) では、FIFO で止まらないための O_NONBLOCK などが無い。
const (
	readFlags  = os.O_RDONLY
	dirFlags   = os.O_RDONLY
	writeFlags = os.O_WRONLY | os.O_CREATE
)

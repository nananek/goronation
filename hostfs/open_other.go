//go:build !unix

package hostfs

import "os"

// unix 以外 (対応 OS は Linux と macOS だけ) では、FIFO で止まらないための O_NONBLOCK が無い。
const readFlags = os.O_RDONLY

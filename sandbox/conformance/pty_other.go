//go:build !linux

package conformance

import (
	"fmt"
	"os"
	"runtime"
)

// openPTY は、この OS には pty の用意が無いので、error を返す (pty_<OS>.go を足す。macOS は、/dev/ptmx と TIOCPTYGRANT・TIOCPTYUNLK・TIOCPTYGNAME)。
func openPTY() (master, slave *os.File, err error) {
	return nil, nil, fmt.Errorf("pty の用意が、%s に無い (pty_%s.go を足す)", runtime.GOOS, runtime.GOOS)
}

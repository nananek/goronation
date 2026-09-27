//go:build !unix

package contract

import "errors"

// closeOnExecFrom は、unix 以外では対応せず、常に error を返す (継承した fd を止められないので、起動しない)。
func closeOnExecFrom(first int) error {
	return errors.New("継承した fd を close-on-exec にできない (unix 以外は対応しない)")
}

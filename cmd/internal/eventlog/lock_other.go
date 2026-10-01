//go:build !unix

package eventlog

import (
	"errors"
	"os"
)

const oNoFollow = 0

func tryLock(*os.File) error { return errors.ErrUnsupported }

func ownedByMe(os.FileInfo) bool { return false }

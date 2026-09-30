//go:build !linux

package main

import (
	"fmt"
	"io"
)

func runLandlockExec(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "goronation landlock-exec: Linux だけ")
	return exitLockdown
}

// exitLockdown は、制限を掛けられず、EXE を起動しなかったときの終了コード。
const exitLockdown = 124

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

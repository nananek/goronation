//go:build !linux

package main

import (
	"fmt"
	"io"
)

// Linux 以外では、run・export・sessions は使えない (檻は bwrap で作る。macOS は後続)。

func runRun(args []string, stderr io.Writer) int { return unsupported("run", stderr) }

func runExport(args []string, stdout, stderr io.Writer) int { return unsupported("export", stderr) }

func runSessions(args []string, stdout, stderr io.Writer) int {
	return unsupported("sessions", stderr)
}

func unsupported(sub string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "goro %s: Linux だけで使える\n", sub)
	return 1
}

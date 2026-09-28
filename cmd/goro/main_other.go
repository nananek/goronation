//go:build !linux

package main

import (
	"fmt"
	"io"
	"os"
)

// Linux 以外では、run・export・sessions は使えない (檻は bwrap で作る。macOS は後続)。

func runRun(args []string, stderr io.Writer) int { return unsupported("run", stderr) }

func runExport(args []string, stdout, stderr io.Writer) int { return unsupported("export", stderr) }

func runSessions(args []string, stdout, stderr io.Writer) int {
	return unsupported("sessions", stderr)
}

func runAuth(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	return unsupported("auth", stderr)
}

func runPr(args []string, stdout, stderr io.Writer) int { return unsupported("pr", stderr) }

func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return unsupported("mcp", stderr)
}

func runServe(args []string, stdout, stderr io.Writer) int { return unsupported("serve", stderr) }

func runWeb(args []string, stdout, stderr io.Writer) int { return unsupported("web", stderr) }

func unsupported(sub string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "goro %s: Linux だけで使える\n", sub)
	return 1
}

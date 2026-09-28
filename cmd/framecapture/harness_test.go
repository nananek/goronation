//go:build linux

package main

import (
	"bytes"
	"errors"
	"os/exec"
	"syscall"
	"testing"
)

func TestExitCodeOfNil(t *testing.T) {
	if code := exitCodeOf(nil, &bytes.Buffer{}); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
}

func TestExitCodeOfNonExitError(t *testing.T) {
	var stderr bytes.Buffer
	code := exitCodeOf(errors.New("何かの別の error (*exec.ExitError ではない)"), &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("*exec.ExitError でない error なのに、stderr に何も書いていない")
	}
}

func TestExitCodeOfExitCode(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "exit 7").Run()
	var stderr bytes.Buffer
	code := exitCodeOf(err, &stderr)
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}
}

func TestExitCodeOfSignaled(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "kill -TERM $$").Run()
	var stderr bytes.Buffer
	code := exitCodeOf(err, &stderr)
	want := 128 + int(syscall.SIGTERM)
	if code != want {
		t.Fatalf("code = %d, want %d (128+SIGTERM)", code, want)
	}
}

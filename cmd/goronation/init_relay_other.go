//go:build !linux

package main

import (
	"errors"
	"os"
	"os/exec"
)

type relayRun struct{}

func prepareRelay(*exec.Cmd, int) (*relayRun, error) {
	return nil, errors.New("--relay-control は linux だけ")
}
func (*relayRun) started(func()) {}
func (*relayRun) close()         {}
func signalTerm(*os.Process)     {}

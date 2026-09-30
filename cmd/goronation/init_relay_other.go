//go:build !linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

type relayRun struct{}

func prepareRelay(*exec.Cmd, int) (*relayRun, error) {
	return nil, errors.New("--relay-control は linux だけ")
}
func (*relayRun) started(func()) {}
func (*relayRun) close()         {}
func (*relayRun) stop()          {}
func signalTerm(*os.Process)     {}
func signalKill(*os.Process)     {}

const relayKillGrace = 5 * time.Second

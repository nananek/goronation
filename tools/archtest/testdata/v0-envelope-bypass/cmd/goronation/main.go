package main

import (
	"github.com/nananek/goronation/agent/claude"
	"github.com/nananek/goronation/core/agent"
)

// spec/v0 を import せずに、Raw を含む Envelope を手に入れて、返す (型は推論で決まる)。
func leak(line []byte) []byte {
	var s agent.Stream = claude.Adapter{}.NewStream()
	envs, _ := s.DecodeFrame(line)
	return envs[0].Raw
}

package egress

import "os"

type runner struct{}

func (runner) StartProcess() {}

func f() {
	os := runner{}
	os.StartProcess()
}

var _ = os.Args

package egress

import (
	"os"
	_ "unsafe"
)

//go:linkname startProcess os.StartProcess
func startProcess(name string, argv []string, attr *os.ProcAttr) (*os.Process, error)

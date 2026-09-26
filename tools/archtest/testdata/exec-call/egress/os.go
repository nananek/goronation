package egress

import "os"

func run() {
	os.StartProcess("/bin/true", nil, nil)
}

package egress

import . "syscall"

func run() error {
	return Exec("/bin/true", nil, nil)
}

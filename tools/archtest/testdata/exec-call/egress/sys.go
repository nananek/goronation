package egress

import sc "syscall"

func run() error {
	return sc.Exec("/bin/true", nil, nil)
}

package egress

import "golang.org/x/sys/unix"

func run() error {
	return unix.Exec("/bin/true", nil, nil)
}

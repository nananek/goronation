//go:build !linux

package main

import "errors"

// setNonDumpable は、Linux 以外では使えない (檻は Linux の bwrap だけ)。
func setNonDumpable() error { return errors.New("Linux だけ") }

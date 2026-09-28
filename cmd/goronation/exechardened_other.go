//go:build !linux

package main

// setDumpableOff は、Linux 以外では何もしない (dumpable は Linux (Yama LSM) 固有の概念で、対応する仕組みが無い)。
func setDumpableOff() error { return nil }

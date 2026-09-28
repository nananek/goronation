//go:build linux

package main

import "syscall"

// prSetDumpable は、Linux の <sys/prctl.h> の PR_SET_DUMPABLE。Go の syscall パッケージには定数が無いので、自前で持つ
// (closefd_linux.go が close_range 用に行っているのと同じ手法。安定した ABI 番号)。
const prSetDumpable = 4

// setDumpableOff は、自分自身に prctl(PR_SET_DUMPABLE, 0) を設定する。この値は、この呼び出し元プロセス自身が
// 以後 execve しない限りは有効 (同一 uid の兄弟プロセスから、capability (CAP_SYS_PTRACE) なしに ptrace・procfs
// (/proc/<pid>/fd) 経由でアクセスされない)。ただし、通常の (特権の変わらない) execve は dumpable を 1 に戻すため、
// この関数の呼び出し直後に別の実行ファイルへ execve すると、dumpable=0 は成り代わった先には引き継がれない
// (exechardened.go の execHardenedUsage を参照。既知の限界)。
func setDumpableOff() error {
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, uintptr(prSetDumpable), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

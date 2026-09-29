//go:build linux

package main

import "syscall"

// setNonDumpable は、自分 (プロセス) を dumpable=0 にする。kernel は、同じ uid の別のプロセスによる、この process の
// ptrace・pidfd_getfd・process_vm_*・/proc/<pid>/mem の開き直しを、許可検査 (ptrace_may_access) で断る (CAP_SYS_PTRACE が無ければ)。
// exec で dumpable に戻る (読めない実行ファイル・setuid を除く)。fork では引き継がれる。
func setNonDumpable() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
		return e
	}
	return nil
}

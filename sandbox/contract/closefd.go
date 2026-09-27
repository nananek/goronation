package contract

// CloseOnExecFrom は、first 以降のすべての fd を close-on-exec にする (fd は閉じない。exec で閉じる)。呼び手が継承した (CLOEXEC でない) fd が、
// 檻のコマンドに届かないようにする。呼び手のプロセス全体に効く。できなければ error (fail-closed): 継承した fd が届くまま、起動しない。
// Linux は close_range と /proc/self/fd、それ以外の unix は /dev/fd を使う。unix 以外は、対応しない (常に error)。
func CloseOnExecFrom(first int) error { return closeOnExecFrom(first) }

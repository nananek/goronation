package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

const execHardenedUsage = `使い方: goronation __exec-hardened -- CMD [ARGS...]

内部専用のサブコマンド (利用者は直接使わない)。goronation init が、子を fork した直後、CMD へ execve で
成り代わる前に、これを起動する。自分自身に prctl(PR_SET_DUMPABLE, 0) を設定してから、CMD (PATH を検索する。
os/exec.LookPath と同じ規則) に execve で成り代わる。

既知の限界 (要検討・未解決): Linux は、特権が変わらない通常の execve では、dumpable を都度 1 に戻す
(prctl(2) の PR_SET_DUMPABLE の項: "Normally, the dumpable attribute is set to 1"。setuid/setgid の実行
ファイルや、permitted capability が増える file capabilities 付きの実行ファイルを exec するときだけ、
suid_dumpable の値になる。CMD (claude・opencode などの通常の実行ファイル) は、そのどれにも当たらない)。
そのため、ここで dumpable=0 を設定してから execve しても、成り代わった CMD 自身の dumpable は 1 に戻り、
0 は引き継がれない (実機で確認済み)。この prctl は、CMD 自身が execve せずに動き続ける限りは意味を持つが、
CMD (エージェント本体) を保護する目的では、現状、効果が無い。
`

// runExecHardened は、goronation __exec-hardened の本体。args は ["--", CMD, ARGS...] の形。
// 自分に dumpable=0 を設定してから、CMD に execve で成り代わる。成功すれば戻らない。
func runExecHardened(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "--" {
		fmt.Fprint(stderr, execHardenedUsage)
		return exitUsage
	}
	argv := args[1:]
	if len(argv) == 0 {
		fmt.Fprint(stderr, execHardenedUsage)
		return exitUsage
	}
	if err := setDumpableOff(); err != nil {
		fmt.Fprintf(stderr, "goronation __exec-hardened: prctl(PR_SET_DUMPABLE, 0) に失敗した: %v\n", err)
		return exitInit
	}
	resolved, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(stderr, "goronation __exec-hardened: %v\n", err)
		return execFailureCode(err)
	}
	err = syscall.Exec(resolved, argv, os.Environ())
	fmt.Fprintf(stderr, "goronation __exec-hardened: execve できない: %v\n", err)
	return execFailureCode(err)
}

// execFailureCode は、CMD を解決・execve できない error を、goronation init と同じ終了コードに変換する
// (見つからないは exitNotFound、それ以外 (実行できない・権限が無いなど) は exitNoExec)。
func execFailureCode(err error) int {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return exitNotFound
	}
	return exitNoExec
}

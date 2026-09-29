//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// unreadableExeMode は、chat セッションで、エージェントの実行ファイルの複製に付ける mode: 所有者は実行だけ (読めない)。
const unreadableExeMode = 0o100

// unreadableExeCopy は、エージェントの実行ファイル exe (ホストの絶対 path・symlink 解決済み) の、読めない複製 (mode 0100) を、dir の下に
// 作って (すでにあれば作らずに) その path を返す。
//
// 理由 (承認フローの完全性。ADR 0010 の B3): kernel は、読めない実行ファイルを exec したプロセスを、dumpable=0 にする
// (fs/exec.c の would_dump)。dumpable=0 のプロセスは、同じ uid の別のプロセス (檻の中の、エージェントの子) による、ptrace・
// pidfd_getfd・process_vm_*・/proc/<pid>/mem の開き直しを、許可検査で断る (CAP_SYS_PTRACE が無ければ)。prctl(PR_SET_DUMPABLE) は、
// exec で dumpable に戻るので、エージェントの実行ファイルの起動の側では、これしか手が無い。実物の claude (native) は、読めなくても動く
// (実測。自分自身を /proc/self/exe から読み直さない)。
//
// 複製は、exe の (path・大きさ・更新時刻) から名前を決めるので、同じ実体なら作り直さない。dir の、ほかの名前 (古い版の複製) は消す。
// 同時に呼ばれても、複製は一時ファイルに書いて rename するので、壊れたものを見せない。
func unreadableExeCopy(dir, exe string) (string, error) {
	fi, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s は通常のファイルではない", exe)
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d", exe, fi.Size(), fi.ModTime().UnixNano()))
	name := hex.EncodeToString(sum[:16])
	dst := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if ci, err := os.Lstat(dst); err == nil && ci.Mode() == unreadableExeMode && ci.Size() == fi.Size() {
		return dst, nil
	}
	src, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // rename 済みなら、何もしない
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(unreadableExeMode); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != name && e.Name()[0] != '.' {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return dst, nil
}

//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// resolveExe は、実行ファイル p を、symlink を辿った絶対 path にする。通常のファイルで、実行できなければ
// error (cmd/goronation/run.go の resolveExe と同じ規則)。
func resolveExe(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s は、実行できる通常のファイルではない", real)
	}
	return real, nil
}

// isScript は、path のファイルの先頭が #! か (読めなければ false)。
func isScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [2]byte
	_, _ = io.ReadFull(f, head[:])
	return head[0] == '#' && head[1] == '!'
}

// findBin は、name (claude・opencode・goronation) の実行ファイルを、flagVal (--*-bin)、なければ
// PATH の name、の順に決め、symlink を辿った実体にする。スクリプト (先頭が #!) は、檻の中では
// 呼ぶ先の実体が見えず動かないので断る (cmd/goronation/run.go の resolveAgentExe と同じ規則)。
func findBin(flagVal, name string) (string, error) {
	path := flagVal
	if path == "" {
		found, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("%s が見つからない。PATH に置くか、--%s-bin PATH で指す", name, name)
		}
		path = found
	}
	real, err := resolveExe(path)
	if err != nil {
		return "", fmt.Errorf("%s (%s) を使えない: %w", name, path, err)
	}
	if isScript(real) {
		return "", fmt.Errorf("%s (%s) はスクリプトで、檻の中では動かない。実体を --%s-bin PATH で指す", name, real, name)
	}
	return real, nil
}

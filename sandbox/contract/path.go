package contract

import (
	"fmt"
	"path/filepath"
	"strings"
)

// CheckPath は、p が絶対・クリーンで、制御文字を含まない path かを確かめる。
func CheckPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("path が空")
	case !filepath.IsAbs(p):
		return fmt.Errorf("path %q が絶対ではない", p)
	case HasControl(p):
		return fmt.Errorf("path %q が制御文字を含む", p)
	case filepath.Clean(p) != p:
		return fmt.Errorf("path %q がクリーンではない (%q)", p, filepath.Clean(p))
	}
	return nil
}

// HasControl は、s が制御文字 (NUL・改行・DEL を含む) を含むか。
func HasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// Under は、p が root と等しいか、root の下にあるか (どちらもクリーンな絶対 path)。
func Under(p, root string) bool {
	return root == "/" || p == root || strings.HasPrefix(p, root+"/")
}

// Overlap は、a と b が、等しいか、どちらかがどちらかの下にあるか。
func Overlap(a, b string) bool {
	return Under(a, b) || Under(b, a)
}

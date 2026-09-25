//go:build unix

package archtest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestNonRegularSource は、.go / go.mod / go.work の名前のファイルが、通常のファイルでは
// ないもの (FIFO など) や、そういうものを指す symlink でも、黙って無視せず error に
// することを確認する。
//
// go tool は、FIFO でも開いて読み、build に使う (実測: FIFO を指す .go の symlink を
// 置いて writer を走らせると、go list の GoFiles に入り、import も読まれた)。
// archtest が見ないまま go が使うと、規則の抜け道になる (fail-closed)。
func TestNonRegularSource(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		for _, name := range []string{"evil.go", "go.mod", "go.work"} {
			kind := "fifo"
			if symlink {
				kind = "symlink"
			}
			t.Run(kind+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				writeTree(t, root, map[string]string{"core/doc.go": "package core\n"})
				fifo := filepath.Join(root, "core", "fifo")
				if err := syscall.Mkfifo(fifo, 0o600); err != nil {
					t.Skipf("FIFO を作れない: %v", err)
				}
				if symlink {
					if err := os.Symlink("fifo", filepath.Join(root, "core", name)); err != nil {
						t.Skipf("symlink を作れない: %v", err)
					}
				} else if err := os.Rename(fifo, filepath.Join(root, "core", name)); err != nil {
					t.Fatal(err)
				}
				if _, _, err := Check(root, DefaultRules); err == nil {
					t.Errorf("%s の %s は error にすべき (go tool は開いて build に使う)", kind, name)
				}
			})
		}
	}
}

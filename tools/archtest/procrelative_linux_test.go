//go:build linux

package archtest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProcessRelativeSymlink は、プロセスごとに別のファイルに解決される symlink (/proc/self/cwd など) を、
// build に使われるファイル (.go・go.mod・go.work) の名前で置いても、検査が空振りしないことを確認する。
//
// archtest は、symlink を「link の位置のファイル」として、テストのプロセス (go test の cwd は tools/archtest) から
// 開いて検査する。一方、go tool・gofmt・compile は別のプロセスで、cwd が違う (make check は、module の dir へ cd して
// go を回し、gofmt は root で回す)。/proc/self/cwd/x は、開いたプロセスの cwd の x を指すため、archtest には無害なファイル、
// go tool には os/exec や replace を含むファイルに見える。ADR 0001 の「symlink の .go は link の位置のファイルとして検査する。
// 追わないと、symlink を置くだけで規則をすり抜けられる」が破れる (実測: make check が exit 0 のまま、core が os/exec に依存した)。
//
// このテストはコードを実行しない。同じ symlink が、cwd によって別のファイルとして読めること (前提) を確かめたうえで、
// archtest がそれを無害と判定せず、違反にするか error にすることだけを検査する。
func TestProcessRelativeSymlink(t *testing.T) {
	const (
		modLine  = "module github.com/nananek/goronation/core\n\ngo 1.24.0\n"
		evilGo   = "package core\n\nimport \"os/exec\"\n\nvar Spawn = exec.Command\n"
		benignGo = "package core\n"
	)
	cases := []struct {
		name    string
		target  string // symlink の指す先
		link    string // root からの、symlink の path
		benign  string // archtest のプロセス (cwd = tools/archtest) から見える実体
		evil    string // go tool のプロセス (cwd = link のある dir) から見える実体
		baseMap map[string]string
	}{
		{"go/proc-self-cwd", "/proc/self/cwd/x.txt", "core/link.go", benignGo, evilGo,
			map[string]string{"core/doc.go": "package core\n"}},
		{"go/proc-thread-self-cwd", "/proc/thread-self/cwd/x.txt", "core/link.go", benignGo, evilGo,
			map[string]string{"core/doc.go": "package core\n"}},
		{"go.mod/proc-self-cwd", "/proc/self/cwd/x.txt", "core/go.mod", modLine, modLine + "\nreplace example.com/x => ./y\n",
			map[string]string{"core/doc.go": "package core\n"}},
		{"go.work/proc-self-cwd", "/proc/self/cwd/x.txt", "go.work", "go 1.24.0\n\nuse ./core\n", "go 1.24.0\n\nuse ./core\n\nreplace example.com/x => ./y\n",
			map[string]string{"core/doc.go": "package core\n", "core/go.mod": modLine}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			linkDir := filepath.Dir(tc.link) // go tool が cwd にする dir
			files := map[string]string{
				filepath.ToSlash(filepath.Join(linkDir, "x.txt")): tc.evil,   // go tool の見る実体 (.txt なので archtest は読まない)
				"tools/archtest/x.txt":                            tc.benign, // archtest のプロセスの見る実体
			}
			for k, v := range tc.baseMap {
				files[k] = v
			}
			writeTree(t, root, files)
			link := filepath.Join(root, filepath.FromSlash(tc.link))
			if err := os.Symlink(tc.target, link); err != nil {
				t.Skipf("symlink を作れない: %v", err)
			}

			// 前提: 同じ symlink が、cwd によって別のファイルに解決される (読むだけ。何も実行しない)。
			read := func(cwd string) string {
				t.Chdir(cwd)
				b, err := os.ReadFile(link)
				if err != nil {
					t.Skipf("この環境では、symlink が cwd 相対に解決されない: %v", err)
				}
				return string(b)
			}
			if got := read(filepath.Join(root, linkDir)); got != tc.evil {
				t.Fatalf("go tool の cwd (%s) で読めた内容が想定と違う: %q", linkDir, got)
			}
			if got := read(filepath.Join(root, "tools", "archtest")); got != tc.benign {
				t.Fatalf("archtest の cwd で読めた内容が想定と違う: %q", got)
			}

			// archtest のプロセスとして検査する (cwd は、いま tools/archtest)。
			vs, _, err := Check(root, DefaultRules)
			if err == nil && len(vs) == 0 {
				t.Errorf("%s は、プロセスごとに別のファイルに解決される (%s)。違反も error も出さずに通してはいけない", tc.link, tc.target)
			}
		})
	}

	// 対照: 通常の相対 symlink (root 内の実体) は、これまでどおり、違反にも error にもしない。
	t.Run("control/relative-symlink", func(t *testing.T) {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, root, map[string]string{"core/doc.go": "package core\n", "core/real.go": "package core\n"})
		if err := os.Symlink("real.go", filepath.Join(root, "core", "link.go")); err != nil {
			t.Skipf("symlink を作れない: %v", err)
		}
		vs, _, err := Check(root, DefaultRules)
		if err != nil || len(vs) != 0 {
			t.Errorf("通常の相対 symlink は通すべき: err=%v violations=%v", err, keys(vs))
		}
	})
}

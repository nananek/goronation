//go:build unix

package archtest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// procTree は、core/doc.go だけを持つ root を作る。
func procTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{"core/doc.go": "package core\n"})
	return root
}

// linkAt は、root/rel に、target を指す symlink を作る。
func linkAt(t *testing.T, root, rel, target string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
}

// wantProcessDependentError は、Check が、プロセスごとに別のものに解決される symlink として、error にすることを確認する。
func wantProcessDependentError(t *testing.T, root, rel string) {
	t.Helper()
	_, _, err := Check(root, DefaultRules)
	if err == nil {
		t.Fatalf("%s は、error にすべき (プロセスごとに別のものに解決される)", rel)
	}
	if !strings.Contains(err.Error(), rel) || !strings.Contains(err.Error(), "プロセスごとに別のものに解決される") {
		t.Errorf("error が、対象の path と理由を含まない: %v", err)
	}
}

// TestProcessDependentTargets は、/proc・/dev・/sys の下を直接指す symlink (build に使われる名前) が、error になることを確認する。
// 名前は .go・go.mod・go.work。/proc は self・thread-self・pid・fd・root・exe、/dev は fd・stdin・stdout・stderr・null・shm。
// /proc・/dev・/sys のディレクトリそのものを指す symlink は、ディレクトリの symlink として、別の規則で error になる。
func TestProcessDependentTargets(t *testing.T) {
	targets := []string{
		"/proc/self/cwd/x", "/proc/thread-self/cwd/x", "/proc/12345/cwd/x", "/proc/self/fd/0", "/proc/self/root/etc/passwd",
		"/proc/self/exe", "/dev/fd/3", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/null", "/dev/shm/x",
		"/sys/kernel/x",
	}
	for _, target := range targets {
		t.Run("evil.go"+target, func(t *testing.T) {
			root := procTree(t)
			linkAt(t, root, "core/evil.go", target)
			wantProcessDependentError(t, root, "core/evil.go")
		})
	}
	for _, name := range []string{"go.mod", "go.work"} {
		for _, target := range []string{"/proc/self/cwd/x", "/dev/null", "/sys/kernel/x"} {
			t.Run(name+target, func(t *testing.T) {
				root := procTree(t)
				linkAt(t, root, "core/"+name, target)
				wantProcessDependentError(t, root, "core/"+name)
			})
		}
	}
}

// TestProcessDependentChains は、symlink の連鎖の、どの段が /proc・/dev・/sys を通っても error になることを確認する
// (連鎖の途中の段の target を、最初の段だけでなく、各段で見る)。相対の target が、../ で root の外に出て /proc に至る場合、
// symlink のディレクトリ (走査しない名前) を通って、字面には /proc が現れない場合も、解決の途中で検出する。
func TestProcessDependentChains(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{"2 段 (相対 → 絶対)", func(t *testing.T, root string) {
			linkAt(t, root, "core/link.go", "hop")
			linkAt(t, root, "core/hop", "/proc/self/cwd/x")
		}},
		{"4 段 (途中は相対と絶対の混在)", func(t *testing.T, root string) {
			linkAt(t, root, "core/link.go", "h1")
			linkAt(t, root, "core/h1", filepath.Join(root, "core", "h2"))
			linkAt(t, root, "core/h2", "../core/h3")
			linkAt(t, root, "core/h3", "/dev/fd/3")
		}},
		{"相対の target が root の外に出て /proc に至る", func(t *testing.T, root string) {
			linkAt(t, root, "core/link.go", strings.Repeat("../", 40)+"proc/self/cwd/x")
		}},
		{"相対の target が root の外に出て /dev に至る (連鎖の途中)", func(t *testing.T, root string) {
			linkAt(t, root, "core/link.go", "hop")
			linkAt(t, root, "core/hop", strings.Repeat("../", 40)+"dev/fd/3")
		}},
		{"走査しない名前の symlink のディレクトリ (/proc) を通る (字面には /proc が無い)", func(t *testing.T, root string) {
			linkAt(t, root, "core/_p", "/proc")
			linkAt(t, root, "core/link.go", "_p/self/cwd/x.txt")
		}},
		{"走査しない名前の symlink のディレクトリ (/dev) を通る連鎖", func(t *testing.T, root string) {
			linkAt(t, root, "core/.d", "/dev")
			linkAt(t, root, "core/hop", ".d/fd/3")
			linkAt(t, root, "core/link.go", "hop")
		}},
		{"走査しない名前の symlink のディレクトリ (/sys) の下の .. ", func(t *testing.T, root string) {
			linkAt(t, root, "core/testdata", "/sys/kernel")
			linkAt(t, root, "core/link.go", "testdata/../x")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := procTree(t)
			tc.setup(t, root)
			wantProcessDependentError(t, root, "core/link.go")
		})
	}
}

// TestProcessDependentControls は、これまでどおり通すもの (error にも違反にもしない) を確認する。
// root の中の実体への symlink (相対・絶対・連鎖)、root の外の通常のファイルへの絶対 symlink (検査される)、
// build に使われない名前の symlink。
func TestProcessDependentControls(t *testing.T) {
	root := procTree(t)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, outside, map[string]string{"ok.go": "package core\n"})
	writeTree(t, root, map[string]string{"core/real.go": "package core\n"})
	linkAt(t, root, "core/rel.go", "real.go")
	linkAt(t, root, "core/abs.go", filepath.Join(root, "core", "real.go"))
	linkAt(t, root, "core/c1.go", "c2")
	linkAt(t, root, "core/c2", "c3")
	linkAt(t, root, "core/c3", "real.go")
	linkAt(t, root, "core/out.go", filepath.Join(outside, "ok.go"))
	linkAt(t, root, "core/link.txt", "/proc/self/cwd/x") // build に使われない名前
	vs, _, err := Check(root, DefaultRules)
	if err != nil || len(vs) != 0 {
		t.Errorf("通すべき: err=%v violations=%v", err, keys(vs))
	}
}

// TestProcessDependentRootUnderDev は、root 自体が /dev の下 (/dev/shm など) にあっても、root の中の symlink を
// 誤って error にせず、/dev の他の場所を指すものは error にすることを確認する。/dev/shm を使えなければ skip する。
func TestProcessDependentRootUnderDev(t *testing.T) {
	base, err := os.MkdirTemp("/dev/shm", "archtest")
	if err != nil {
		t.Skipf("/dev/shm を使えない: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{"core/doc.go": "package core\n", "core/real.go": "package core\n"})
	linkAt(t, root, "core/rel.go", "real.go")
	linkAt(t, root, "core/abs.go", filepath.Join(root, "core", "real.go"))
	if vs, _, err := Check(root, DefaultRules); err != nil || len(vs) != 0 {
		t.Fatalf("root の中の symlink は通すべき: err=%v violations=%v", err, keys(vs))
	}
	linkAt(t, root, "core/fd.go", "/dev/fd/3")
	wantProcessDependentError(t, root, "core/fd.go")
}

// TestProcessDependentLoop は、symlink の輪 (連鎖が戻ってくる) で、判定が終わらなくならないことを確認する。
// 輪の .go・go.mod は、Stat の失敗で error になる。輪の vendor/modules.txt は、go も読めずに失敗するので、
// 違反にしない。Check を別 goroutine で回し、止まったら失敗にする (checkNoHang)。
func TestProcessDependentLoop(t *testing.T) {
	for _, name := range []string{"evil.go", "go.mod"} {
		t.Run(name, func(t *testing.T) {
			root := procTree(t)
			linkAt(t, root, "core/"+name, "hop")
			linkAt(t, root, "core/hop", name)
			if _, err := checkNoHang(t, root); err == nil {
				t.Error("error を返すべき")
			}
		})
	}
	t.Run("vendor/modules.txt", func(t *testing.T) {
		root := procTree(t)
		linkAt(t, root, "vendor/modules.txt", "hop")
		linkAt(t, root, "vendor/hop", "modules.txt")
		vs, err := checkNoHang(t, root)
		if err != nil || len(vs) != 0 {
			t.Errorf("通すべき: err=%v violations=%v", err, keys(vs))
		}
	})
}

// TestInProcessDependentRoots は、判定の境界 (/proc と /procx、root の中の除外) を確認する。
func TestInProcessDependentRoots(t *testing.T) {
	cases := []struct {
		p, root string
		want    bool
	}{
		{"/proc", "/work/repo", true},
		{"/proc/self/cwd/x", "/work/repo", true},
		{"/procx/self", "/work/repo", false},
		{"/dev/fd/3", "/work/repo", true},
		{"/devices/x", "/work/repo", false},
		{"/sys/kernel/x", "/work/repo", true},
		{"/system/x", "/work/repo", false},
		{"/work/repo/core/x.go", "/work/repo", false},
		{"/dev/shm/repo/core/x.go", "/dev/shm/repo", false},
		{"/dev/shm/repo", "/dev/shm/repo", false},
		{"/dev", "/dev/shm/repo", false},     // root の祖先
		{"/dev/shm", "/dev/shm/repo", false}, // root の祖先
		{"/dev/shm/repox/core", "/dev/shm/repo", true},
		{"/dev/shm/other", "/dev/shm/repo", true},
		{"/dev/fd", "/dev/shm/repo", true},
		{"/dev/fd/3", "/dev/shm/repo", true},
		{"/proc/self/cwd/x", "/dev/shm/repo", true},
	}
	for _, tc := range cases {
		if got := inProcessDependentRoots(tc.p, tc.root); got != tc.want {
			t.Errorf("inProcessDependentRoots(%q, %q) = %v, want %v", tc.p, tc.root, got, tc.want)
		}
	}
}

// TestProcessDependentVendor は、vendor と vendor/modules.txt が、プロセスごとに別のものに解決される symlink なら、
// Stat の結果によらず (archtest の cwd では存在せず、壊れた symlink に見えても) vendor-mode の違反にすることを確認する。
// go は、modules.txt があると、vendor の中の外部 module を build する。
func TestProcessDependentVendor(t *testing.T) {
	want := []string{"vendor/modules.txt:1: vendor-mode"}
	for name, setup := range map[string]func(t *testing.T, root string){
		"modules.txt が /proc/self/cwd を指す": func(t *testing.T, root string) {
			linkAt(t, root, "vendor/modules.txt", "/proc/self/cwd/m.txt")
		},
		"modules.txt が連鎖の先で /proc を通る": func(t *testing.T, root string) {
			linkAt(t, root, "vendor/modules.txt", "hop")
			linkAt(t, root, "vendor/hop", "/dev/fd/3")
		},
		"vendor が /proc/self/cwd を指す (壊れた symlink に見える)": func(t *testing.T, root string) {
			linkAt(t, root, "vendor", "/proc/self/cwd/v")
		},
		"vendor が /dev を指す": func(t *testing.T, root string) {
			linkAt(t, root, "vendor", "/dev")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := procTree(t)
			setup(t, root)
			vs, _, err := Check(root, DefaultRules)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(keys(vs), want) {
				t.Errorf("violations=%v (want %v)", keys(vs), want)
			}
		})
	}

	t.Run("対照: vendor が root の中のディレクトリへの symlink で、modules.txt が無ければ違反にしない", func(t *testing.T) {
		root := procTree(t)
		writeTree(t, root, map[string]string{"real/x.txt": "x\n"})
		linkAt(t, root, "vendor", "real")
		vs, _, err := Check(root, DefaultRules)
		if err != nil || len(vs) != 0 {
			t.Errorf("通すべき: err=%v violations=%v", err, keys(vs))
		}
	})
}

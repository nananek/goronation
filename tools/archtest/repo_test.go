package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// repoAnchors は、repo の root を正しく取れていれば、必ず走査される .go。archtest 自身と core。
var repoAnchors = []string{"tools/archtest/archtest.go", "core/doc.go"}

// repoRoot は、go test の cwd (tools/archtest のパッケージ dir) から、repo の root (2 つ上) を返す。
// root は、cwd の絶対 path (論理 path) の 2 つ上で、symlink を解決しない。cwd が <root>/tools/archtest と
// 一致しなければ error にする。さらに、<root>/tools と <root>/tools/archtest が symlink なら error にする
// (go は、symlink の module の dir を、解決せずに cwd にする)。
//   - cwd から最も近い go.work を root にする方式は、tools/archtest に nested の go.work を置くだけで、
//     root をすり替えられた (走査が tools/archtest だけになり、core の os/exec が通った)。
//   - symlink を解決して root を作る方式は、tools/archtest を repo 内の decoy への symlink に置き換えるだけで、
//     root を decoy にすり替えられた (decoy が core/doc.go と archtest.go を持てば、repoAnchors も通った)。
func repoRoot(cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd %q が絶対 path ではない", cwd)
	}
	dir := filepath.Clean(cwd)
	root := filepath.Dir(filepath.Dir(dir))
	if dir != filepath.Join(root, "tools", "archtest") {
		return "", fmt.Errorf("cwd %s が <root>/tools/archtest ではない", dir)
	}
	for _, p := range []string{filepath.Join(root, "tools"), dir} {
		info, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s が symlink (root のすり替えを防ぐため、symlink は許さない)", p)
		}
	}
	return root, nil
}

// missingFiles は、want のうち、files (走査した .go の repo 相対 path) に無いものを返す。
func missingFiles(files []string, want ...string) []string {
	var missing []string
	for _, w := range want {
		if !slices.Contains(files, w) {
			missing = append(missing, w)
		}
	}
	return missing
}

// TestRepoRoot は、root が go test の cwd の 2 つ上 (論理 path。symlink を解決しない) に固定され、
// root/tools と root/tools/archtest が symlink なら error になることを確認する。
// cwd から最も近い go.work を root にする方式は、tools/archtest に nested の go.work を置くだけで、
// root を tools/archtest にすり替えられた (走査が tools/archtest だけになり、core の os/exec が通った)。
// symlink を解決して root を作る方式は、tools/archtest を repo 内の decoy への symlink に置き換えるだけで、
// root を decoy にすり替えられた (core の os/exec が通った)。
func TestRepoRoot(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, tmp, map[string]string{
		"repo/go.work":                                   "go 1.24.0\n\nuse ./tools/archtest\n",
		"repo/core/doc.go":                               "package core\n",
		"repo/tools/archtest/archtest.go":                "package archtest\n",
		"repo/tools/archtest/go.work":                    "go 1.24.0\n\nuse .\n", // nested の go.work (decoy)
		"repo/tools/archtest/go.mod":                     "module github.com/nananek/goronation\n",
		"repo/tools/archtest/tools/archtest/archtest.go": "package archtest\n", // decoy
		"repo/tools/other/x.go":                          "package other\n",
	})
	repo := filepath.Join(tmp, "repo")
	symlink := func(t *testing.T, target, link string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink を作れない: %v", err)
		}
	}

	t.Run("decoy があっても、cwd の 2 つ上", func(t *testing.T) {
		got, err := repoRoot(filepath.Join(repo, "tools", "archtest"))
		if err != nil {
			t.Fatal(err)
		}
		if got != repo {
			t.Errorf("repoRoot = %q, want %q", got, repo)
		}
	})

	t.Run("root への symlink 経由の cwd は、解決せずに論理 path の 2 つ上 (root 自体の symlink は許す)", func(t *testing.T) {
		link := filepath.Join(tmp, "replink")
		symlink(t, repo, link)
		got, err := repoRoot(filepath.Join(link, "tools", "archtest"))
		if err != nil {
			t.Fatal(err)
		}
		if got != link {
			t.Errorf("repoRoot = %q, want %q (論理 path)", got, link)
		}
	})

	t.Run("tools/archtest が別の場所への symlink なら error", func(t *testing.T) {
		writeTree(t, tmp, map[string]string{"elsewhere/x/archtest.go": "package archtest\n"})
		symlink(t, filepath.Join(tmp, "elsewhere", "x"), filepath.Join(tmp, "repo2", "tools", "archtest"))
		if got, err := repoRoot(filepath.Join(tmp, "repo2", "tools", "archtest")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	t.Run("tools が symlink なら error", func(t *testing.T) {
		writeTree(t, tmp, map[string]string{"elsewhere2/tools/archtest/archtest.go": "package archtest\n"})
		symlink(t, filepath.Join(tmp, "elsewhere2", "tools"), filepath.Join(tmp, "repo3", "tools"))
		if got, err := repoRoot(filepath.Join(tmp, "repo3", "tools", "archtest")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	// tools/archtest を、repo 内の decoy への symlink に置き換える。decoy が core/doc.go と
	// tools/archtest/archtest.go を持てば、repoAnchors も満たせるので、symlink を解決して root を作ると、
	// root が decoy になり、core の os/exec が通る。
	t.Run("tools/archtest が repo 内の decoy への symlink なら error", func(t *testing.T) {
		writeTree(t, tmp, map[string]string{
			"repo4/core/exec.go":                          "package core\n\nimport \"os/exec\"\n\nvar Spawn = exec.Command\n",
			"repo4/docs/decoy/core/doc.go":                "package core\n",
			"repo4/docs/decoy/tools/archtest/archtest.go": "package archtest\n",
		})
		symlink(t, filepath.Join("..", "docs", "decoy", "tools", "archtest"), filepath.Join(tmp, "repo4", "tools", "archtest"))
		if got, err := repoRoot(filepath.Join(tmp, "repo4", "tools", "archtest")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	t.Run("tools/archtest が無ければ error", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(tmp, "repo5", "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got, err := repoRoot(filepath.Join(tmp, "repo5", "tools", "archtest")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	t.Run("cwd が tools/archtest でなければ error", func(t *testing.T) {
		cases := map[string]string{
			"repo の root":           repo,
			"tools":                 filepath.Join(repo, "tools"),
			"tools/other":           filepath.Join(repo, "tools", "other"),
			"tools/archtest の 1 つ下": filepath.Join(repo, "tools", "archtest", "tools"),
			"core":                  filepath.Join(repo, "core"),
			"ファイルシステムの root":        string(filepath.Separator),
		}
		for name, cwd := range cases {
			if got, err := repoRoot(cwd); err == nil {
				t.Errorf("%s: error を返すべき (root = %q)", name, got)
			}
		}
	})

	t.Run("相対 path は error", func(t *testing.T) {
		if got, err := repoRoot("."); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})
}

// TestRootConfusionIsDetected は、root を取り違えても、走査した .go に archtest 自身と core が含まれない
// ことで検知できる (二重の防御) ことを確認する。decoy の tools/archtest/tools/archtest/archtest.go だけでは、
// archtest 自身の存在チェックは通っても、core が走査されない。
func TestRootConfusionIsDetected(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, tmp, map[string]string{
		"repo/core/doc.go":                               "package core\n",
		"repo/tools/archtest/archtest.go":                "package archtest\n",
		"repo/tools/archtest/tools/archtest/archtest.go": "package archtest\n", // decoy
	})
	scanFrom := func(root string) []string {
		t.Helper()
		_, files, err := scan(root, DefaultRules)
		if err != nil {
			t.Fatal(err)
		}
		return files
	}

	if got := missingFiles(scanFrom(filepath.Join(tmp, "repo")), repoAnchors...); len(got) != 0 {
		t.Errorf("正しい root なのに、走査されていない .go = %v", got)
	}
	confused := filepath.Join(tmp, "repo", "tools", "archtest") // tools/archtest を root と取り違える
	if got, want := missingFiles(scanFrom(confused), repoAnchors...), []string{"core/doc.go"}; !slices.Equal(got, want) {
		t.Errorf("取り違えた root で、走査されていない .go = %v, want %v", got, want)
	}
}

func TestMissingFiles(t *testing.T) {
	files := []string{"core/doc.go", "tools/archtest/archtest.go", "sandbox/x.go"}
	cases := []struct {
		want, missing []string
	}{
		{[]string{"core/doc.go"}, nil},
		{[]string{"core/doc.go", "tools/archtest/archtest.go"}, nil},
		{[]string{"core/doc.go", "core/nope.go", "x.go"}, []string{"core/nope.go", "x.go"}},
		{nil, nil},
	}
	for _, tc := range cases {
		if got := missingFiles(files, tc.want...); !slices.Equal(got, tc.missing) {
			t.Errorf("missingFiles(%v) = %v, want %v", tc.want, got, tc.missing)
		}
	}
	if got := missingFiles(nil, repoAnchors...); !slices.Equal(got, repoAnchors) {
		t.Errorf("何も走査していないとき = %v, want %v", got, repoAnchors)
	}
}

// TestRepository は、このリポジトリ自体が規則を満たすことを確認する。
// archtest 自身も対象で、自己免除しない。
func TestRepository(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := repoRoot(wd)
	if err != nil {
		t.Fatalf("repo の root を決められない (go test の cwd は tools/archtest のはず): %v", err)
	}

	violations, files, err := scan(root, DefaultRules)
	if err != nil {
		t.Fatalf("Check がエラーを返した: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("走査した .go が 0 ファイル (空振りで緑にしない)")
	}
	// root の取り違えの二重の防御。archtest 自身と core が走査されていなければ、root が違う。
	if missing := missingFiles(files, repoAnchors...); len(missing) != 0 {
		t.Fatalf("走査した .go に %v が無い (root を取り違えている: %s)", missing, root)
	}
	for _, v := range violations {
		t.Errorf("%s", v)
	}
	t.Logf("%d 個の .go を走査した (root=%s)", len(files), root)
}

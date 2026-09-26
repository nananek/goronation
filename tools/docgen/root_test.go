package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepoRoot は、root が cwd の 2 つ上 (論理 path。symlink を解決しない) に固定され、
// root/tools と root/tools/docgen が symlink なら error になることを確認する。
// go.work を上向きに探す方式は、入れ子の go.work を置くだけで、root をすり替えられた。
// symlink を解決して root を作る方式は、tools/docgen を repo 内の decoy への symlink に置き換えるだけで、
// root を decoy にすり替えられた (tools/archtest の TestRepoRoot と同じ)。
func TestRepoRoot(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, tmp, map[string]string{
		"repo/go.work":                                "go 1.24.0\n\nuse ./tools/docgen\n",
		"repo/tools/docgen/main.go":                   "package main\n",
		"repo/tools/docgen/go.work":                   "go 1.24.0\n\nuse .\n", // 入れ子の go.work (decoy)
		"repo/tools/docgen/tools/docgen/decoy.go":     "package main\n",
		"repo/tools/other/x.go":                       "package other\n",
		"repo4/docs/decoy/tools/docgen/go.mod":        "module decoy\n",
		"repo4/docs/decoy/go.work":                    "go 1.24.0\n",
		"elsewhere/x/main.go":                         "package main\n",
		"elsewhere2/tools/docgen/main.go":             "package main\n",
		"repo5/tools/keep":                            "",
		"repo4/tools/placeholder-so-tools-dir-exists": "",
	})
	repo := filepath.Join(tmp, "repo")

	t.Run("入れ子の go.work や decoy があっても、cwd の 2 つ上", func(t *testing.T) {
		got, err := repoRoot(filepath.Join(repo, "tools", "docgen"))
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
		got, err := repoRoot(filepath.Join(link, "tools", "docgen"))
		if err != nil {
			t.Fatal(err)
		}
		if got != link {
			t.Errorf("repoRoot = %q, want %q (論理 path)", got, link)
		}
	})

	t.Run("tools/docgen が別の場所への symlink なら error", func(t *testing.T) {
		symlink(t, filepath.Join(tmp, "elsewhere", "x"), filepath.Join(tmp, "repo2", "tools", "docgen"))
		if got, err := repoRoot(filepath.Join(tmp, "repo2", "tools", "docgen")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	t.Run("tools が symlink なら error", func(t *testing.T) {
		symlink(t, filepath.Join(tmp, "elsewhere2", "tools"), filepath.Join(tmp, "repo3", "tools"))
		if got, err := repoRoot(filepath.Join(tmp, "repo3", "tools", "docgen")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	t.Run("tools/docgen が repo 内の decoy への symlink なら error", func(t *testing.T) {
		symlink(t, filepath.Join("..", "docs", "decoy", "tools", "docgen"), filepath.Join(tmp, "repo4", "tools", "docgen"))
		if got, err := repoRoot(filepath.Join(tmp, "repo4", "tools", "docgen")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	t.Run("tools/docgen が無ければ error", func(t *testing.T) {
		if got, err := repoRoot(filepath.Join(tmp, "repo5", "tools", "docgen")); err == nil {
			t.Errorf("error を返すべき (root = %q)", got)
		}
	})

	t.Run("cwd が tools/docgen でなければ error", func(t *testing.T) {
		cases := map[string]string{
			"repo の root":         repo,
			"tools":               filepath.Join(repo, "tools"),
			"tools/other":         filepath.Join(repo, "tools", "other"),
			"tools/docgen の 1 つ下": filepath.Join(repo, "tools", "docgen", "tools"),
			"tools/archtest":      filepath.Join(repo, "tools", "archtest"),
			"ファイルシステムの root":      string(filepath.Separator),
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

// TestOpenRepo は、root に go.work と tools/docgen/go.mod (通常のファイル) が無ければ error になることを確認する
// (root の取り違えの二重の防御)。
func TestOpenRepo(t *testing.T) {
	t.Run("正しい root", func(t *testing.T) {
		root := fixtureRepo(t, nil)
		r, err := openRepo(filepath.Join(root, "tools", "docgen"))
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
	})

	t.Run("go.work が無い", func(t *testing.T) {
		root := fixtureRepo(t, nil)
		if err := os.Remove(filepath.Join(root, "go.work")); err != nil {
			t.Fatal(err)
		}
		if r, err := openRepo(filepath.Join(root, "tools", "docgen")); err == nil {
			r.Close()
			t.Error("error を返すべき")
		}
	})

	t.Run("go.work が通常のファイルではない (ディレクトリ)", func(t *testing.T) {
		root := fixtureRepo(t, nil)
		if err := os.Remove(filepath.Join(root, "go.work")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "go.work"), 0o755); err != nil {
			t.Fatal(err)
		}
		if r, err := openRepo(filepath.Join(root, "tools", "docgen")); err == nil {
			r.Close()
			t.Error("error を返すべき")
		}
	})

	t.Run("go.work が symlink", func(t *testing.T) {
		root := fixtureRepo(t, map[string]string{"real.work": "go 1.24.0\n"})
		if err := os.Remove(filepath.Join(root, "go.work")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "real.work", filepath.Join(root, "go.work"))
		if r, err := openRepo(filepath.Join(root, "tools", "docgen")); err == nil {
			r.Close()
			t.Error("error を返すべき")
		}
	})

	t.Run("tools/docgen/go.mod が無い", func(t *testing.T) {
		root := fixtureRepo(t, nil)
		if err := os.Remove(filepath.Join(root, "tools", "docgen", "go.mod")); err != nil {
			t.Fatal(err)
		}
		if r, err := openRepo(filepath.Join(root, "tools", "docgen")); err == nil {
			r.Close()
			t.Error("error を返すべき")
		}
	})

	t.Run("cwd が違う", func(t *testing.T) {
		root := fixtureRepo(t, nil)
		if r, err := openRepo(root); err == nil {
			r.Close()
			t.Error("error を返すべき")
		}
	})
}

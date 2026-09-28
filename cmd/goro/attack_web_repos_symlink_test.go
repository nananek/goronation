//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveRepoNameRejectsSymlinkEscapingReposDir は、攻撃者視点レビュー (attack-review-d848c34) の
// 再現テスト。--repos-dir 配下に、外を指す symlink (名前自体は "evil-link" で、".." もスラッシュも
// 含まない、字面上は完全に正当な 1 要素) を置いたとき、resolveRepoName (POST /api/repos/start が使う)
// が、それを受理して、reposDir の外を指す path を返してしまうことを確かめる。
//
// resolveRepoName は filepath.IsLocal で字面上のトラバーサル (".."・絶対 path) は断るが、これは
// レキシカルな (ファイルシステムに触れない) 検査でしかなく、symlink によるエスケープは防げない。
// このテストは、resolveRepoName が返す path を実際に filepath.EvalSymlinks し、reposDir の実体の
// 外を指していないことを期待する。現状の実装では、この期待は満たされない。
func TestResolveRepoNameRejectsSymlinkEscapingReposDir(t *testing.T) {
	reposDir := t.TempDir()
	outside := t.TempDir() // reposDir とは無関係な、別の一時ディレクトリ
	if err := os.MkdirAll(filepath.Join(outside, "secret-repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret-repo"), filepath.Join(reposDir, "evil-link")); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveRepoName(reposDir, "evil-link")
	if err != nil {
		return // 断られれば、それが期待どおりの (安全な) 挙動
	}
	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		t.Fatalf("resolveRepoName が返した path (%s) を EvalSymlinks できない: %v", resolved, err)
	}
	realReposDir, err := filepath.EvalSymlinks(reposDir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(real) != realReposDir {
		t.Fatalf("resolveRepoName(%q, \"evil-link\") = %q は、symlink をたどると reposDir (実体 %s) の外 "+
			"(%s) を指す。POST /api/repos/start に \"evil-link\" を渡すと、goro serve --repo %s が起動され、"+
			"reposDir の外の repo がそのまま複製・操作対象になる (実際に到達するには、reposDir 配下に、"+
			"外を指す symlink があらかじめ存在する必要がある)。",
			reposDir, resolved, realReposDir, filepath.Dir(real), resolved)
	}
}

// TestListReposExcludesSymlinkedGitMetadata は、.git 自体が symlink である場合 (repo の実体は別の
// 場所にあり、reposDir にはその .git への symlink だけを置くような構成) に、listRepos (GET /api/repos)
// が、それを一覧から除外することを期待する。os.Stat は symlink をたどるため、現状の実装では、
// トップレベルのディレクトリ名自体は reposDir 配下の本物のディレクトリであっても、中の .git が外を
// 指す symlink であれば、一覧に含まれてしまう (実際に選択されて起動されると、git がその symlink を
// たどり、reposDir の外の内容を操作対象にする)。
func TestListReposExcludesSymlinkedGitMetadata(t *testing.T) {
	reposDir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(reposDir, "looks-normal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, ".git"), filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}

	names, err := listRepos(reposDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == "looks-normal" {
			t.Fatalf(".git が (reposDir の外を指す) symlink であるディレクトリ %q が、一覧に含まれてしまった。"+
				"選択すると、goro serve --repo %s が起動され、git は symlink をたどって reposDir の外の "+
				"リポジトリを対象にする。", "looks-normal", dir)
		}
	}
}

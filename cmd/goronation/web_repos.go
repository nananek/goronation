//go:build linux

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
)

// listRepos は、reposDir 直下のディレクトリのうち、git repo (直下に .git がある) のものだけを、
// 名前の昇順で返す (goronation-web-plan §4-1: 深い階層はたどらない。1 段だけの一覧)。reposDir が空なら
// (--repos-dir を指定していない)、常に空を返す。.git の実在確認は、symlink をたどった実体が
// reposDir の外を指していないことまで確かめる (攻撃者視点レビュー attack-review-d848c34 の B1:
// os.Stat だけでは、.git 自体が reposDir の外を指す symlink であるディレクトリを、そのまま一覧に
// 含めてしまう。中身は本物の repo に見えないが、選ぶと git がその symlink をたどって操作対象にする)。
func listRepos(reposDir string) ([]string, error) {
	if reposDir == "" {
		return nil, nil
	}
	realReposDir, err := filepath.EvalSymlinks(reposDir)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(reposDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue // symlink 自体は DirEntry.IsDir() が false を返す (先が dir でも false): パターン 1 はここで除外
		}
		dirPath := filepath.Join(reposDir, e.Name())
		realDirPath, err := filepath.EvalSymlinks(dirPath)
		if err != nil || !withinDir(realDirPath, realReposDir) {
			continue
		}
		realGitPath, err := filepath.EvalSymlinks(filepath.Join(dirPath, ".git"))
		if err != nil {
			continue // .git が無い、または symlink の先が存在しない
		}
		if !withinDir(realGitPath, realDirPath) {
			continue // .git が symlink で、repo 自身 (realDirPath) の外を指す (パターン 2)
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// resolveRepoName は、reposDir 直下の名前 name を、実際の repo の path にする。name が
// filepath.IsLocal でない (path 区切り・".." などを含み、reposDir の外を指しうる) ときは断る。
// name 自体、または name が指す先の途中に symlink があり、実体が reposDir の外を指す場合も断る
// (攻撃者視点レビュー attack-review-d848c34 の B1: filepath.IsLocal は文字面だけのレキシカルな検査
// で、symlink によるエスケープは防げない。filepath.EvalSymlinks で実体を解決してから、reposDir の
// 実体の配下であることを確かめる)。
func resolveRepoName(reposDir, name string) (string, error) {
	if reposDir == "" {
		return "", errReposDirNotConfigured
	}
	if name == "" || !filepath.IsLocal(name) {
		return "", errBadRepoName
	}
	path := filepath.Join(reposDir, name)
	realReposDir, err := filepath.EvalSymlinks(reposDir)
	if err != nil {
		return "", errBadRepoName
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errBadRepoName
	}
	if !withinDir(realPath, realReposDir) {
		return "", errBadRepoName
	}
	return path, nil
}

// withinDir は、path (実体解決済み) が、dir (実体解決済み) そのもの、または dir の直下であるかを返す
// (どちらも filepath.EvalSymlinks 済みであることを呼び手が保証する)。
func withinDir(path, dir string) bool {
	return path == dir || filepath.Dir(path) == dir
}

var (
	errReposDirNotConfigured = httpError{status: http.StatusNotFound, label: "repo のファイルブラウザは使えない (--repos-dir なしで起動した)"}
	errBadRepoName           = httpError{status: http.StatusBadRequest, label: "repo の名前が不正"}
)

// httpError は、writeError にそのまま渡せる、状態コードと外へ出してよい label を持つ error。
type httpError struct {
	status int
	label  string
}

func (e httpError) Error() string { return e.label }

// handleReposList は、GET /api/repos: reposDir 直下の git repo の名前を、JSON 配列で返す。
func handleReposList(reposDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reposDir == "" {
			writeJSON(w, http.StatusOK, []string{}) // ファイルブラウザ機能自体が無いだけ (§4-2)。空の一覧にする
			return
		}
		names, err := listRepos(reposDir)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "repo の一覧を取得できない")
			return
		}
		writeJSON(w, http.StatusOK, names)
	}
}

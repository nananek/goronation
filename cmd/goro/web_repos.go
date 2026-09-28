//go:build linux

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
)

// listRepos は、reposDir 直下のディレクトリのうち、git repo (直下に .git がある) のものだけを、
// 名前の昇順で返す (goro-web-plan §4-1: 深い階層はたどらない。1 段だけの一覧)。reposDir が空なら
// (--repos-dir を指定していない)、常に空を返す。
func listRepos(reposDir string) ([]string, error) {
	if reposDir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(reposDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(reposDir, e.Name(), ".git")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// resolveRepoName は、reposDir 直下の名前 name を、実際の repo の path にする。name が
// filepath.IsLocal でない (path 区切り・".." などを含み、reposDir の外を指しうる) ときは断る。
func resolveRepoName(reposDir, name string) (string, error) {
	if reposDir == "" {
		return "", errReposDirNotConfigured
	}
	if name == "" || !filepath.IsLocal(name) {
		return "", errBadRepoName
	}
	return filepath.Join(reposDir, name), nil
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

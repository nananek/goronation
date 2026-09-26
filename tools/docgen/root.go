package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// repoAnchors は、repo の root を正しく取れていれば、必ずある通常のファイル。
var repoAnchors = []string{"go.work", "tools/docgen/go.mod"}

// repoRoot は、cwd (<root>/tools/docgen) から、repo の root (2 つ上) を返す。
// root は、cwd の論理 path の 2 つ上で、symlink を解決しない。cwd が <root>/tools/docgen と一致しなければ
// error にする。さらに、<root>/tools と <root>/tools/docgen が symlink なら error にする。
// tools/archtest の repoRoot と同じ考え方で、次のすり替えを防ぐ。
//   - go.work を上向きに探す方式は、入れ子の go.work を置くだけで、root をすり替えられた。
//   - symlink を解決して root を作る方式は、tools/docgen を repo 内の decoy への symlink に置き換えるだけで、
//     root を decoy にすり替えられた。
func repoRoot(cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd %q が絶対 path ではない", cwd)
	}
	dir := filepath.Clean(cwd)
	root := filepath.Dir(filepath.Dir(dir))
	if dir != filepath.Join(root, "tools", "docgen") {
		return "", fmt.Errorf("cwd %s が <root>/tools/docgen ではない", dir)
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

// openRepo は、cwd から root を決めて、os.Root として開く。以降の読み書きは、すべてこの Root の内側で行う。
// root に repoAnchors が無ければ error にする (root の取り違えの二重の防御)。
func openRepo(cwd string) (*os.Root, error) {
	root, err := repoRoot(cwd)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	for _, a := range repoAnchors {
		info, err := r.Lstat(a)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("root %s に %s が無い (root を取り違えている): %w", root, a, err)
		}
		if !info.Mode().IsRegular() {
			r.Close()
			return nil, fmt.Errorf("root %s の %s が通常のファイルではない (root を取り違えている)", root, a)
		}
	}
	return r, nil
}

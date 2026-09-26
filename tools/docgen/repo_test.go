package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pendingFindings は、この repo の、docs-check が、まだ報告する問題 (README と ADR 0001 を薄くし、archtest の package doc の
// 見出しを直す後続の変更で、なくなる)。これ以外の問題が出たら、この test が赤になる。なくなった項目は、一覧から消す。
var pendingFindings = []string{
	"README.md md-size",
	"docs/adr/0001-repository-layout.md md-size",
	"tools/archtest/doc.go pkg-doc-heading",
}

// copyRepo は、実際の repo (root) を、tmp にコピーする。.git と bin は除く。symlink は、そのまま (辿らずに) 写す。
func copyRepo(t *testing.T, root, dst string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" || rel == "bin" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, out)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o644)
		}
		return nil // FIFO など: コピーしない (docgen が error にする場面は、別のテストが見る)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRepository は、この repo 自体を、docgen で生成し、検査する (偽陽性の検出)。repo を tmp にコピーして (repo を
// 書き換えない)、ADR の索引の生成区間の枠が無ければ足す (枠を置くのは、後続の変更)。
//   - 生成が、error なく終わる。
//   - lint の問題は、pendingFindings だけ。
//   - 生成した後の照合には、問題が無い (冪等。生成物と索引が、コードと一致する)。
func TestRepository(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := repoRoot(wd)
	if err != nil {
		t.Fatalf("repo の root を決められない (go test の cwd は tools/docgen のはず): %v", err)
	}
	dst, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	copyRepo(t, root, dst)

	readme := filepath.Join(dst, "docs", "adr", "README.md")
	b, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(b); !strings.Contains(text, adrBegin) {
		i := strings.Index(text, "| 番号 |")
		j := strings.Index(text[i:], "\n\n")
		if i < 0 || j < 0 {
			t.Fatal("docs/adr/README.md の索引の表が見つからない (テストの前提)")
		}
		text = text[:i] + adrBegin + "\n" + adrEnd + text[i+j:]
		if err := os.WriteFile(readme, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tr := newTree(openTestRoot(t, dst), defaultLimits)
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"core", "sandbox/bwrap", "cmd/goro", "tools/archtest", "tools/docgen"} {
		if !slices.ContainsFunc(res.Docs, func(d *pkgDoc) bool { return d.Src.Dir == want }) {
			t.Errorf("package %s の文書が無い", want)
		}
	}
	for _, k := range keysOf(res.Findings) {
		if !slices.Contains(pendingFindings, k) {
			t.Errorf("想定していない問題: %s", k)
		}
	}
	for _, p := range pendingFindings {
		if !slices.Contains(keysOf(res.Findings), p) {
			t.Logf("pendingFindings の %q は、もう報告されない (一覧から消してよい)", p)
		}
	}

	if _, _, err := tr.writeOutputs(res); err != nil {
		t.Fatal(err)
	}
	tr2 := newTree(openTestRoot(t, dst), defaultLimits)
	res2, err := tr2.analyze()
	if err != nil {
		t.Fatal(err)
	}
	cmp, err := tr2.compare(res2)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range cmp {
		t.Errorf("生成した直後の照合に、問題がある: %s", f)
	}

	// CLI (run) でも、同じ結果になり、-check は何も書かない。
	before := readTree(t, dst)
	code, out, errs := runIn(t, filepath.Join(dst, "tools", "docgen"), "-check")
	if code != 1 {
		t.Errorf("-check の code = %d, want 1 (pendingFindings がある間)", code)
	}
	for _, line := range strings.Split(strings.TrimSpace(errs), "\n") {
		if strings.HasPrefix(line, "docgen: ") {
			continue
		}
		if !slices.ContainsFunc(pendingFindings, func(p string) bool {
			path, r, _ := strings.Cut(p, " ")
			return strings.HasPrefix(line, path) && strings.Contains(line, "["+r+"]")
		}) {
			t.Errorf("-check の想定していない出力: %s", line)
		}
	}
	_ = out
	if after := readTree(t, dst); len(after) != len(before) {
		t.Error("-check がファイルを増減させた")
	} else {
		for p, c := range before {
			if after[p] != c {
				t.Errorf("-check が %s を書き換えた", p)
			}
		}
	}
}

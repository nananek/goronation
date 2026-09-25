package archtest

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// buildIgnored は、//go:build ignore のファイル (どの GOOS / GOARCH でも build されない、生成用の
// package main) か。標準ライブラリには、os/exec を使う生成用のファイルが多数あり、数えると過大になる。
func buildIgnored(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:build ") {
				x, err := constraint.Parse(c.Text)
				return err == nil && x.Eval(func(tag string) bool { return tag == "ignore" })
			}
		}
	}
	return false
}

// stdImports は、GOROOT/src の package (import path) ごとに、import する path の集合を返す。
// 全ビルドタグ・_test.go 以外の全ファイルが対象 (archtest と同じ、os/exec は使わず go/parser で読む)。
// cmd/ は import できないので除く。src/vendor (標準ライブラリが使う golang.org/x/...) は、中継として読む。
func stdImports(src string) (map[string]map[string]bool, error) {
	imports := map[string]map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && rel != "vendor" && (skipDir(d.Name()) || rel == "cmd") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		if buildIgnored(f) {
			return nil
		}
		pkg := path.Dir(rel)
		if imports[pkg] == nil {
			imports[pkg] = map[string]bool{}
		}
		for _, spec := range f.Imports {
			ipath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			imports[pkg][ipath] = true
		}
		return nil
	})
	return imports, err
}

// execDependents は、os/exec に (推移的に) 依存する package の集合を返す。
func execDependents(imports map[string]map[string]bool) map[string]bool {
	// 標準ライブラリのソースは "golang.org/x/..." と書くが、実体は vendor/golang.org/x/... にある。
	resolve := func(p string) string {
		if _, ok := imports[p]; !ok {
			if _, ok := imports["vendor/"+p]; ok {
				return "vendor/" + p
			}
		}
		return p
	}
	importers := map[string][]string{}
	for pkg, set := range imports {
		for ipath := range set {
			p := resolve(ipath)
			importers[p] = append(importers[p], pkg)
		}
	}
	deps := map[string]bool{}
	stack := []string{"os/exec"}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, importer := range importers[p] {
			if !deps[importer] {
				deps[importer] = true
				stack = append(stack, importer)
			}
		}
	}
	return deps
}

// TestStdImportsVendor は、GOROOT/src/vendor (標準ライブラリが使う golang.org/x/...) も走査し、
// vendor の package を経由した os/exec への依存も数えることを確認する。走査しないと、標準ライブラリの
// package が vendor の package 経由で os/exec に依存するようになっても、TestStdExecDependents は気づけない。
func TestStdImportsVendor(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"os/exec/exec.go":            "package exec\n",
		"vendor/golang.org/x/v/v.go": "package v\n\nimport _ \"os/exec\"\n",
		"vendor/testdata/t/t.go":     "package t\n\nimport _ \"os/exec\"\n",
		"p/p.go":                     "package p\n\nimport _ \"golang.org/x/v\"\n",
		"q/q.go":                     "package q\n",
	})
	imports, err := stdImports(src)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(execDependents(imports)))
	if want := []string{"p", "vendor/golang.org/x/v"}; !slices.Equal(got, want) {
		t.Errorf("os/exec に依存する package = %v, want %v", got, want)
	}
}

// TestStdExecDependents は、標準ライブラリで os/exec に (推移的に) 依存する、import できる package が、
// exec-import の表 (execImports) に全部載っていることを確認する。標準ライブラリの中の import は
// exec-import の対象外なので、net/http/cgi のような package を import するだけで、子プロセスを起動できる。
// Go を更新して、そういう package が増えると、このテストが赤になり、表に足すべき package を示す。
//
// fail-closed: GOROOT の src を読めない・解析が空振りしている場合は、skip せずに失敗する。
func TestStdExecDependents(t *testing.T) {
	goroot := os.Getenv("GOROOT")
	if goroot == "" {
		goroot = runtime.GOROOT() // -trimpath で build した場合は空
	}
	if goroot == "" {
		t.Fatal("GOROOT を決められない。環境変数 GOROOT を設定して実行する")
	}
	// Debian などは src が symlink で、filepath.WalkDir は root の symlink を辿らない。
	src, err := filepath.EvalSymlinks(filepath.Join(goroot, "src"))
	if err != nil {
		t.Fatalf("GOROOT の src を読めない: %v", err)
	}
	imports, err := stdImports(src)
	if err != nil {
		t.Fatalf("GOROOT の src を解析できない: %v", err)
	}
	deps := execDependents(imports)
	if len(imports) < 100 || len(deps) == 0 {
		t.Fatalf("解析が空振りしている (%s: package %d 個、os/exec に依存する package %d 個)", src, len(imports), len(deps))
	}

	i := slices.IndexFunc(DefaultRules.AllowImports, func(r AllowImport) bool { return r.ID == "exec-import" })
	if i < 0 {
		t.Fatal("規則 exec-import が表に無い")
	}
	rule := DefaultRules.AllowImports[i]

	var missing []string
	for p := range deps {
		// vendor/ と internal/ は、標準ライブラリの外から import できない (中継としては数える)。
		if strings.HasPrefix(p, "vendor/") || slices.Contains(strings.Split(p, "/"), "internal") {
			continue
		}
		if !matchAny(rule.Imports, p) {
			missing = append(missing, p)
		}
	}
	slices.Sort(missing)
	for _, p := range missing {
		t.Errorf("標準ライブラリの %s は os/exec に依存するが、execImports (rules.go) に無い", p)
	}
	if len(missing) == 0 {
		t.Logf("%s: os/exec に依存する、import できる標準ライブラリの package は、全部表に載っている (中継を含めて %d 個)", src, len(deps))
	}
}

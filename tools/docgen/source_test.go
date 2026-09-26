package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// forbiddenOS は、tree を通さずに、ファイルシステム・プロセス・標準入出力・環境に触れる os の名前。
// 書き込みだけでなく、読みも、root の外や symlink の先へ出られるので、含める。
var forbiddenOS = []string{
	"WriteFile", "Create", "CreateTemp", "OpenFile", "Open", "ReadFile", "ReadDir", "DirFS", "NewFile",
	"Mkdir", "MkdirAll", "MkdirTemp", "Remove", "RemoveAll", "Rename", "Symlink", "Link", "Readlink",
	"Chmod", "Chown", "Lchown", "Chtimes", "Truncate", "Stat", "Lstat", "Chdir", "Chroot",
	"Getwd", "OpenRoot", "Pipe", "StartProcess", "FindProcess", "Exit",
	"Setenv", "Unsetenv", "Clearenv", "Getenv", "LookupEnv", "Environ", "ExpandEnv", "Expand",
	"Stdin", "Stdout", "Stderr", "Args",
}

// allowedUses は、forbiddenOS の名前の、例外 ("<ファイル>:<関数>")。root と cwd を決める所と、main だけ。
var allowedUses = map[string][]string{
	"Lstat":    {"root.go:repoRoot"},
	"OpenRoot": {"root.go:openRepo"},
	"Getwd":    {"main.go:main"},
	"Args":     {"main.go:main"},
	"Stdout":   {"main.go:main"},
	"Stderr":   {"main.go:main"},
	"Exit":     {"main.go:main"},
}

// rootMethods は、*os.Root の、読み書きする methods の名前。tree の methods と、root を開く所だけが呼ぶ。
var rootMethods = []string{"OpenFile", "Open", "Create", "Mkdir", "Remove", "Lstat", "Stat", "FS", "OpenRoot"}

// allowedImports は、docgen の (テスト以外の) source が import してよい標準ライブラリ。これ以外は、
// import を足す変更が、このテストの変更として、レビューに見える。
func allowedImport(file, path string) bool {
	switch path {
	case "bytes", "errors", "fmt", "io", "io/fs", "net/url", "os", "path", "path/filepath", "regexp", "slices", "strconv", "strings",
		"time", "unicode", "unicode/utf8", "go/ast", "go/doc", "go/doc/comment", "go/parser", "go/printer", "go/scanner", "go/token":
		return true
	case "syscall":
		return file == "open_unix.go" // O_NONBLOCK・O_DIRECTORY の定数だけ
	}
	return false
}

func isTreeMethod(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return false
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == "tree"
}

// TestSourceHygiene は、docgen 自身の (テスト以外の) source を AST で調べる。
//   - 読み書きは、tree (os.Root の内側) だけを通る。os.WriteFile・os.Create・os.OpenFile・os.Mkdir*・os.Remove* などの、
//     tree を通らない入口が無い。
//   - 標準出力・標準エラーへ出す入口は、emit だけ (emit は normalize を通る)。
//   - os/exec・unsafe・reflect・net などを import しない。
func TestSourceHygiene(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var src []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			src = append(src, f)
		}
	}
	if len(src) < 5 {
		t.Fatalf("docgen の source が %v だけ (空振りで緑にしない)", src)
	}

	fset := token.NewFileSet()
	for _, name := range src {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, b, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !allowedImport(name, p) {
				t.Errorf("%s: import %q は、許可していない (許可する import は、source_test.go の allowedImport)", name, p)
			}
		}
		for _, decl := range f.Decls {
			fn := ""
			treeMethod := false
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				treeMethod = isTreeMethod(fd)
			}
			where := name + ":" + fn
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
						t.Errorf("%s: 組み込みの %s は使わない", fset.Position(x.Pos()), id.Name)
					}
					// token.File.Line(pos) も、Position と同じく、//line で補正した行を返す (lineOf を使う)。
					if se, ok := x.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "Line" {
						t.Errorf("%s: .Line(...) は、//line で補正した行を返す。lineOf を使う", fset.Position(x.Pos()))
					}
				case *ast.SelectorExpr:
					pos := fset.Position(x.Pos()).String()
					sel := x.Sel.Name
					if id, ok := x.X.(*ast.Ident); ok {
						switch id.Name {
						case "os":
							if slices.Contains(forbiddenOS, sel) && !slices.Contains(allowedUses[sel], where) {
								t.Errorf("%s: os.%s は、tree を通さない入口 (%s)。許可する場所: %v", pos, sel, where, allowedUses[sel])
							}
						case "filepath":
							if slices.Contains([]string{"Walk", "WalkDir", "EvalSymlinks", "Glob", "Abs"}, sel) {
								t.Errorf("%s: filepath.%s は、root の外を辿りうる", pos, sel)
							}
						case "fmt":
							if (strings.HasPrefix(sel, "Print") || strings.HasPrefix(sel, "Fprint")) && where != "main.go:emit" {
								t.Errorf("%s: fmt.%s は、出口 (emit 以外は、normalize を通らない)", pos, sel)
							}
						case "syscall":
							if sel != "O_NONBLOCK" && sel != "O_DIRECTORY" && sel != "Stat_t" { // Stat_t は、リンク数 (linkCount)
								t.Errorf("%s: syscall.%s は使わない", pos, sel)
							}
						}
					}
					// 行番号は lineOf (補正しない PositionFor) で求める。Position は //line ディレクティブで補正した行を返す。
					if sel == "Position" {
						t.Errorf("%s: .Position は、//line で補正した行を返す。lineOf を使う (%s)", pos, where)
					}
					if sel == "PositionFor" && where != "pkg.go:lineOf" {
						t.Errorf("%s: .PositionFor は、lineOf の中だけ (%s)", pos, where)
					}
					// ファイルへ書く・消す入口は、それぞれ 1 か所だけ (writeOutput が、書く内容の全体を normalize に通す)。
					if sel == "writeFile" && where != "build.go:writeOutput" {
						t.Errorf("%s: writeFile は、writeOutput 以外から呼ばない (%s)", pos, where)
					}
					if sel == "remove" && where != "build.go:writeOutputs" {
						t.Errorf("%s: remove は、writeOutputs 以外から呼ばない (%s)", pos, where)
					}
					// os.X の形は、上で見た。ここは、値のメソッド (t.root.OpenFile など) の呼び出し。
					if id, isIdent := x.X.(*ast.Ident); (!isIdent || id.Name != "os") && slices.Contains(rootMethods, sel) {
						if !(name == "walk.go" && treeMethod) && where != "root.go:openRepo" {
							t.Errorf("%s: .%s は、*os.Root の入口の可能性がある (%s)。tree の methods (walk.go) を通す", pos, sel, where)
						}
					}
				}
				return true
			})
		}
	}
}

// gateCalls は、source の関数 fn (ファイル file) の中の、normalize(kind..., ...) の呼び出しの、種類の名前の集合。
func gateCalls(t *testing.T, file, fn string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	seenFn := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		seenFn = true
		ast.Inspect(fd, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "normalize" {
				if k, ok := call.Args[0].(*ast.Ident); ok {
					found[k.Name] = true
				}
			}
			return true
		})
	}
	if !seenFn {
		t.Fatalf("%s に関数 %s が無い", file, fn)
	}
	return found
}

// TestGatesExist は、出口の関門が、消えていないことを確認する。
// 診断の出口 (emit) は kindDiag、ファイルへ書く入口 (writeOutput) と、文書を組み立てる 2 か所は kindDocument で、
// normalize を通る。
func TestGatesExist(t *testing.T) {
	cases := []struct{ file, fn, kind string }{
		{"main.go", "emit", "kindDiag"},
		{"build.go", "writeOutput", "kindDocument"},
		{"render.go", "render", "kindDocument"},
		{"render.go", "renderRefIndex", "kindDocument"},
		{"render.go", "decl", "kindGoBlock"},
	}
	for _, c := range cases {
		if !gateCalls(t, c.file, c.fn)[c.kind] {
			t.Errorf("%s の %s が、normalize(%s, ...) を通らない", c.file, c.fn, c.kind)
		}
	}
}

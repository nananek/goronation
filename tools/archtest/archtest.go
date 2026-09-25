package archtest

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ruleModPath は、go.mod の module 行の検査の規則 ID。
// import 規則は module path の一致を前提にするため、ずれを検知する。
const ruleModPath = "modpath"

// ruleUnscannedImport は、走査しないディレクトリ (skipDir) を含む、リポジトリ内の import path の規則 ID。
// go tool は、そこにある package も、明示的に import されれば build する。
// 走査しないまま import だけを許すと、そのディレクトリが規則の抜け道になる。
const ruleUnscannedImport = "unscanned-dir-import"

// Violation は規則違反 1 件。
type Violation struct {
	Path   string // repo 相対、"/" 区切り
	Line   int
	Rule   string
	Detail string
}

// String は "<repo相対path>:<line>: <rule-id>: <detail>" 形式にする。
func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", v.Path, v.Line, v.Rule, v.Detail)
}

// AllowImport は「Imports を import してよいのは OnlyIn に合うファイルだけ」という規則 (許可リスト)。
// OnlyIn が空なら、どのファイルも import できない (全面禁止)。
type AllowImport struct {
	ID      string
	Imports []string // import path のパターン
	OnlyIn  []string // repo 相対のファイル path のパターン
}

// ForbidImport は「In に合うファイルは Imports を import できない」という規則。
type ForbidImport struct {
	ID      string
	In      []string // repo 相対のファイル path のパターン
	Imports []string // import path のパターン
}

// Func は import path と関数名の組。
type Func struct {
	Pkg  string
	Name string
}

// CallRule は「Funcs の参照は OnlyIn に合うファイルだけ」という規則。
type CallRule struct {
	ID     string
	Funcs  []Func
	OnlyIn []string // repo 相対のファイル path のパターン
}

// Rules は規則の表。
type Rules struct {
	// Module は、このリポジトリの module path の共通の接頭辞。
	// 各 go.mod の module 行は Module + "/" + <相対dir> と一致しなければならない。
	Module        string
	AllowImports  []AllowImport
	ForbidImports []ForbidImport
	Calls         []CallRule
}

// Check は root 配下の .go と go.mod を検査する。
//
// 検査は構文解析だけで行い (go list を使わない)、全ビルドタグ・全 _test.go を対象にする。
// 走査対象は root 配下の全 .go で、.git / testdata / vendor と、"." または "_" で始まる
// ディレクトリは除く。go tool と同じく、symlink のディレクトリは辿らず、
// symlink の .go と go.mod は link の位置のファイルとして検査する。
//
// fail-closed: 走査した .go が 0 ファイルなら error を返す。構文解析できない .go や、
// 読めないディレクトリも error にする (見えないものを、違反なしとして扱わない)。
func Check(root string, rules Rules) (violations []Violation, scanned int, err error) {
	if rules.Module == "" {
		return nil, 0, errors.New("Rules.Module が空 (import 規則が黙って空振りするため許さない)")
	}
	root, err = filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("%s はディレクトリではない", root)
	}

	c := &checker{rules: rules, fset: token.NewFileSet()}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// go tool は、symlink の .go と go.mod を通常のファイルとして読む
			// (symlink のディレクトリは辿らない)。この 2 つの symlink だけは、指す先を
			// link の位置のファイルとして検査する。壊れた symlink は error にする。
			if name := d.Name(); name != "go.mod" && !strings.HasSuffix(name, ".go") {
				return nil
			}
			target, err := os.Stat(p)
			if err != nil {
				return err
			}
			if !target.Mode().IsRegular() {
				return nil
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case strings.HasSuffix(d.Name(), ".go"):
			c.scanned++
			return c.checkGo(p, rel)
		case d.Name() == "go.mod":
			return c.checkGoMod(p, rel)
		}
		return nil
	})
	if err != nil {
		return nil, c.scanned, err
	}
	if c.scanned == 0 {
		return nil, 0, fmt.Errorf("%s の配下に走査対象の .go が 1 つも無い (空振りで緑にしない)", root)
	}

	slices.SortFunc(c.violations, func(a, b Violation) int {
		return cmp.Or(
			strings.Compare(a.Path, b.Path),
			cmp.Compare(a.Line, b.Line),
			strings.Compare(a.Rule, b.Rule),
			strings.Compare(a.Detail, b.Detail),
		)
	})
	return c.violations, c.scanned, nil
}

type checker struct {
	rules      Rules
	fset       *token.FileSet
	scanned    int
	violations []Violation
}

func (c *checker) add(rel string, line int, rule, detail string) {
	c.violations = append(c.violations, Violation{Path: rel, Line: line, Rule: rule, Detail: detail})
}

// skipDir は走査から除くディレクトリ名か。".git" も "." の接頭辞に含まれる。
func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func (c *checker) checkGo(file, rel string) error {
	f, err := parser.ParseFile(c.fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("%s: 構文解析に失敗: %w", rel, err)
	}

	for _, spec := range f.Imports {
		ipath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("%s: import path を解釈できない: %w", rel, err)
		}
		line := c.fset.Position(spec.Path.Pos()).Line
		c.checkUnscannedImport(rel, line, ipath)
		for _, r := range c.rules.AllowImports {
			if matchAny(r.Imports, ipath) && !matchAny(r.OnlyIn, rel) {
				c.add(rel, line, r.ID, unless(fmt.Sprintf("import %q", ipath), r.OnlyIn))
			}
		}
		for _, r := range c.rules.ForbidImports {
			if matchAny(r.In, rel) && matchAny(r.Imports, ipath) {
				c.add(rel, line, r.ID, fmt.Sprintf("%s から import %q は使えない", strings.Join(r.In, ", "), ipath))
			}
		}
	}

	for _, r := range c.rules.Calls {
		if !matchAny(r.OnlyIn, rel) {
			c.checkCalls(rel, f, r)
		}
	}
	return nil
}

// checkUnscannedImport は、ipath がリポジトリ内の path で、走査しないディレクトリの名前を
// セグメントに含む場合に違反にする。許可される場所は無い。
func (c *checker) checkUnscannedImport(rel string, line int, ipath string) {
	sub, ok := strings.CutPrefix(ipath, c.rules.Module+"/")
	if !ok {
		return
	}
	for _, seg := range strings.Split(sub, "/") {
		if skipDir(seg) {
			c.add(rel, line, ruleUnscannedImport, fmt.Sprintf(
				"import %q は走査しないディレクトリ %q を含む (明示 import されると go tool は build するが、archtest は走査しない)", ipath, seg))
			return
		}
	}
}

// checkCalls は、r.Funcs の参照を検出する。
//
// import の別名は解決する。識別子の名前だけで照合し、スコープは解決しないため、
// 局所変数が import 名を shadow している場合も検出する (誤検出側に倒す = fail-closed)。
// 呼び出しに限らず、関数値としての参照 (f := os.StartProcess) も検出する。
func (c *checker) checkCalls(rel string, f *ast.File, r CallRule) {
	pkgs := map[string]bool{}
	for _, fn := range r.Funcs {
		pkgs[fn.Pkg] = true
	}

	names := map[string]string{} // ファイル内のローカル名 → import path
	for _, spec := range f.Imports {
		ipath, _ := strconv.Unquote(spec.Path.Value) // checkGo で検証済み
		if !pkgs[ipath] {
			continue
		}
		switch {
		case spec.Name == nil:
			names[path.Base(ipath)] = ipath
		case spec.Name.Name == "_": // 参照できないので、判定するものが無い
		case spec.Name.Name == ".":
			// ドット import は非修飾の識別子で参照されるため、判定できない。
			line := c.fset.Position(spec.Path.Pos()).Line
			c.add(rel, line, r.ID, fmt.Sprintf("import . %q は呼び出しを判定できないため、", ipath)+onlyTail(r.OnlyIn))
		default:
			names[spec.Name.Name] = ipath
		}
	}
	if len(names) == 0 {
		return
	}

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := ast.Unparen(sel.X).(*ast.Ident) // (os).StartProcess も同じ参照
		if !ok {
			return true
		}
		ipath, ok := names[id.Name]
		if !ok {
			return true
		}
		for _, fn := range r.Funcs {
			if fn.Pkg == ipath && fn.Name == sel.Sel.Name {
				line := c.fset.Position(sel.Pos()).Line
				c.add(rel, line, r.ID, unless(ipath+"."+fn.Name, r.OnlyIn))
			}
		}
		return true
	})
}

// checkGoMod は、go.mod の module 行が Module + "/" + <相対dir> と一致することを確認する。
func (c *checker) checkGoMod(file, rel string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	want := c.rules.Module
	if dir := path.Dir(rel); dir != "." {
		want += "/" + dir
	}

	for i, line := range strings.Split(string(data), "\n") {
		if j := strings.Index(line, "//"); j >= 0 {
			line = line[:j]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "module" {
			continue
		}
		if len(fields) != 2 {
			c.add(rel, i+1, ruleModPath, "module 行を解釈できない")
			return nil
		}
		if got := strings.Trim(fields[1], "\"`"); got != want {
			c.add(rel, i+1, ruleModPath, fmt.Sprintf("module が %q だが、%q であるべき", got, want))
		}
		return nil
	}
	c.add(rel, 1, ruleModPath, "module 行が無い")
	return nil
}

// unless は「patterns 以外では使えない」旨の detail を作る。patterns が空なら全面禁止。
func unless(subject string, patterns []string) string {
	if len(patterns) == 0 {
		return subject + " は全面禁止"
	}
	return subject + " は " + onlyTail(patterns)
}

// onlyTail は detail の末尾の句を作る。
func onlyTail(patterns []string) string {
	if len(patterns) == 0 {
		return "全面禁止"
	}
	return strings.Join(patterns, ", ") + " 以外では使えない"
}

// match は pattern が p に合うか。pattern は完全一致、または "P/**" (P とその配下、セグメント単位)。
func match(pattern, p string) bool {
	if base, ok := strings.CutSuffix(pattern, "/**"); ok {
		return p == base || strings.HasPrefix(p, base+"/")
	}
	return p == pattern
}

func matchAny(patterns []string, p string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool { return match(pattern, p) })
}

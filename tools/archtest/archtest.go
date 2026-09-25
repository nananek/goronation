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

// ruleNonGoSource は、Go が build に使う、.go 以外のソース (アセンブリ・C など) の規則 ID。
// archtest は .go の構文しか見ない。アセンブリ 1 ファイルで、import も表の関数の参照も無しに
// execve でき、それを検査する手段も無い。そのため、見えないものは禁止し、許可される場所は無い。
const ruleNonGoSource = "non-go-source"

// nonGoSourceExts は、go/build が build に使う、.go 以外のソースの拡張子。
// go/build の (*Context).matchFile の switch と同じで、大文字小文字を区別する。
var nonGoSourceExts = []string{
	".c", ".cc", ".cpp", ".cxx", ".m", ".h", ".hh", ".hpp", ".hxx",
	".f", ".F", ".for", ".f90", ".s", ".S", ".sx", ".swig", ".swigcxx", ".syso",
}

// ruleReplace は、go.mod / go.work の replace の規則 ID。replace は、走査しないディレクトリや、
// 別の module path のコードを、そのまま取り込める (core/go.mod に 1 行足すだけで、core/_evil の
// os/exec に依存できる)。import path を見る unscanned-dir-import では、これを防げない。
// 許可される場所は無い。
const ruleReplace = "replace"

// ruleGoWorkUse は、go.work の use の規則 ID。use は、リポジトリの root の内側で、走査しない
// ディレクトリを含まず、go.mod を持つディレクトリだけを許す。use ./core/_evil で、走査しない
// module を取り込めるため。root の外や、go.mod が無いものも、見えない (検査していない) ので許さない。
const ruleGoWorkUse = "go-work-use"

// ruleNestedGoWork は、repo の root 直下 (go.work) 以外の go.work の規則 ID。go は、cwd から上に向かって
// 最も近い go.work で module を解決する。make と go test の cwd は module の dir とパッケージの dir なので、
// 入れ子の go.work は、workspace を丸ごと差し替えられる (tools/archtest/go.work で、TestRepository の root を
// tools/archtest にすり替えられた)。許可される場所は無い。
const ruleNestedGoWork = "nested-go-work"

// ruleLinkname は、//go:linkname の規則 ID。linkname は、名前を変えて他の package の関数を参照でき、
// セレクタ (os.StartProcess など) で照合する exec-call をすり抜ける。Go 1.24 のリンカは、標準ライブラリの
// 公開していない参照を既定で拒否する (link: invalid reference) が、-checklinkname=0 で回避できる。
// その flag は Makefile や GOFLAGS で渡せるので、リンカの検査には頼らず、ディレクティブ自体を禁止する。
// 位置は問わず (字下げも、関数の中も)、許可される場所は無い。
const ruleLinkname = "linkname"

// ruleVendorMode は、vendor/modules.txt の規則 ID。go は、vendor/modules.txt があると、vendor の中の
// 外部 module を build する (workspace でも同じ。ネットワークも go.sum も要らない)。archtest は vendor を
// 走査しないため、vendor/example.com/evil の os/exec を、core が使える。許可される場所は無い。
const ruleVendorMode = "vendor-mode"

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
// ディレクトリは除く。除いたディレクトリを import する、リポジトリ内の import path は
// 違反にする (unscanned-dir-import)。symlink の .go と go.mod は link の位置のファイルとして
// 検査し、symlink のディレクトリは辿らずに error にする (走査しないディレクトリの名前のものを除く)。
// Go が build に使う .go 以外のソース (.s など。nonGoSourceExts) は、検査できないため、
// あるだけで違反にする (non-go-source)。
//
// fail-closed: 走査した .go が 0 ファイルなら error を返す。構文解析できない .go や、
// 読めないディレクトリ、辿れない symlink も error にする (見えないものを、違反なしとして扱わない)。
func Check(root string, rules Rules) (violations []Violation, scanned int, err error) {
	violations, files, err := scan(root, rules)
	return violations, len(files), err
}

// scan は Check の本体で、走査した .go の repo 相対 path ("/" 区切り、走査した順) も返す。
func scan(root string, rules Rules) (violations []Violation, files []string, err error) {
	if rules.Module == "" {
		return nil, nil, errors.New("Rules.Module が空 (import 規則が黙って空振りするため許さない)")
	}
	root, err = filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("%s はディレクトリではない", root)
	}

	c := &checker{root: root, rules: rules, fset: token.NewFileSet()}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				c.checkVendorMode(p)
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if slices.Contains(nonGoSourceExts, path.Ext(d.Name())) {
			c.add(rel, 1, ruleNonGoSource, d.Name()+" は、Go が build に使う .go 以外のソース (archtest は検査できない) のため、全面禁止")
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// go tool は、symlink の .go と go.mod / go.work を通常のファイルとして読むため、これらの
			// symlink は、指す先を link の位置のファイルとして検査する。壊れていたら error にする。
			// symlink のディレクトリは辿らずに error にする。go tool は、./... では列挙しないが、
			// 明示的に import されれば辿って build するため、黙って無視すると規則の抜け道になる。
			// 走査しないディレクトリの名前のものだけは、実体のディレクトリと同じく走査しない
			// (その import は unscanned-dir-import が違反にする)。
			target, err := os.Stat(p)
			switch {
			case err == nil && target.IsDir():
				if skipDir(d.Name()) {
					c.checkVendorMode(p)
					return nil
				}
				return fmt.Errorf("%s: ディレクトリの symlink は検査できない "+
					"(go tool は明示的に import されると辿って build するため、辿らずに無視すると規則の抜け道になる。"+
					"実体のディレクトリにする)", rel)
			case !isModFile(d.Name()) && !strings.HasSuffix(d.Name(), ".go"):
				return nil // build に使われない。壊れていてもよい
			case err != nil:
				return err
			case !target.Mode().IsRegular():
				return nil
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		switch {
		case strings.HasSuffix(d.Name(), ".go"):
			c.files = append(c.files, rel)
			return c.checkGo(p, rel)
		case d.Name() == "go.mod":
			return c.checkGoMod(p, rel)
		case d.Name() == "go.work":
			return c.checkGoWork(p, rel)
		}
		return nil
	})
	if err != nil {
		return nil, c.files, err
	}
	if len(c.files) == 0 {
		return nil, nil, fmt.Errorf("%s の配下に走査対象の .go が 1 つも無い (空振りで緑にしない)", root)
	}

	slices.SortFunc(c.violations, func(a, b Violation) int {
		return cmp.Or(
			strings.Compare(a.Path, b.Path),
			cmp.Compare(a.Line, b.Line),
			strings.Compare(a.Rule, b.Rule),
			strings.Compare(a.Detail, b.Detail),
		)
	})
	return c.violations, c.files, nil
}

type checker struct {
	root       string
	rules      Rules
	fset       *token.FileSet
	files      []string // 走査した .go の repo 相対 path
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

// checkVendorMode は、走査しないディレクトリ p が vendor で、modules.txt を持つなら違反にする (ruleVendorMode)。
// p が symlink でも、指す先を見る (go は辿る)。
func (c *checker) checkVendorMode(p string) {
	if filepath.Base(p) != "vendor" {
		return
	}
	modules := filepath.Join(p, "modules.txt")
	if info, err := os.Stat(modules); err != nil || !info.Mode().IsRegular() {
		return
	}
	rel, err := filepath.Rel(c.root, modules)
	if err != nil {
		rel = modules
	}
	c.add(filepath.ToSlash(rel), 1, ruleVendorMode,
		"vendor/modules.txt がある (go は vendor から外部 module を build するが、archtest は vendor を走査しない) ため、全面禁止")
}

// isModFile は、go が module の構成に使うファイルの名前か。
func isModFile(name string) bool {
	return name == "go.mod" || name == "go.work"
}

func (c *checker) checkGo(file, rel string) error {
	f, err := parser.ParseFile(c.fset, file, nil, parser.SkipObjectResolution|parser.ParseComments)
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

	for _, cg := range f.Comments {
		for _, cm := range cg.List {
			if strings.HasPrefix(cm.Text, "//go:linkname") {
				c.add(rel, c.fset.Position(cm.Pos()).Line, ruleLinkname, "//go:linkname は全面禁止 (名前を変えて、表の関数を参照できるため)")
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

// readModFile は go.mod / go.work を読み、文に分ける。解釈できなければ error にする。
func readModFile(file, rel string) ([]modStmt, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	stmts, err := parseModFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	return stmts, nil
}

// checkReplace は、replace の文を全部違反にする (ruleReplace)。
func (c *checker) checkReplace(rel string, stmts []modStmt) {
	for _, s := range stmts {
		if s.Verb == "replace" {
			c.add(rel, s.Line, ruleReplace, "replace は全面禁止 (archtest が走査しないコードを、別の module path で取り込めるため)")
		}
	}
}

// checkGoMod は、go.mod の module 行が Module + "/" + <相対dir> と一致することと、
// replace が無いことを確認する。
func (c *checker) checkGoMod(file, rel string) error {
	stmts, err := readModFile(file, rel)
	if err != nil {
		return err
	}
	c.checkReplace(rel, stmts)

	want := c.rules.Module
	if dir := path.Dir(rel); dir != "." {
		want += "/" + dir
	}
	i := slices.IndexFunc(stmts, func(s modStmt) bool { return s.Verb == "module" })
	switch {
	case i < 0:
		c.add(rel, 1, ruleModPath, "module 行が無い")
	case len(stmts[i].Args) != 1:
		c.add(rel, stmts[i].Line, ruleModPath, "module 行を解釈できない")
	case stmts[i].Args[0] != want:
		c.add(rel, stmts[i].Line, ruleModPath, fmt.Sprintf("module が %q だが、%q であるべき", stmts[i].Args[0], want))
	}
	return nil
}

// checkGoWork は、root 直下以外の go.work と、go.work の replace と、使えない use を違反にする。
// go の directive (go / toolchain / godebug / use / replace) 以外は、go も受け付けないため、error にする。
// use の path は、その go.work のあるディレクトリからの相対で解決する (入れ子の go.work も検査する)。
func (c *checker) checkGoWork(file, rel string) error {
	if rel != "go.work" {
		c.add(rel, 1, ruleNestedGoWork, "入れ子の go.work は、workspace を丸ごと差し替えられる (go は cwd に最も近い go.work を使う) ため、全面禁止")
	}
	stmts, err := readModFile(file, rel)
	if err != nil {
		return err
	}
	c.checkReplace(rel, stmts)
	for _, s := range stmts {
		switch s.Verb {
		case "go", "toolchain", "godebug", "replace":
		case "use":
			if len(s.Args) != 1 {
				return fmt.Errorf("%s:%d: go.work の use の引数が 1 つではない", rel, s.Line)
			}
			if why := c.unusable(filepath.Dir(file), s.Args[0]); why != "" {
				c.add(rel, s.Line, ruleGoWorkUse, fmt.Sprintf("use %q は、%s", s.Args[0], why))
			}
		default:
			return fmt.Errorf("%s:%d: go.work の directive %q を解釈できない", rel, s.Line, s.Verb)
		}
	}
	return nil
}

// unusable は、go.work の use が指すディレクトリ (dir からの相対、または絶対) を使えない理由を返す。
// 使えるなら空。".." は、先に字句的に畳んでから判定する (./core/../../x を、core の中と誤らない)。
func (c *checker) unusable(dir, use string) string {
	target := use
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	sub, err := filepath.Rel(c.root, filepath.Clean(target))
	if err != nil {
		return "リポジトリの root の外を指す"
	}
	sub = filepath.ToSlash(sub)
	if sub == ".." || strings.HasPrefix(sub, "../") {
		return "リポジトリの root の外を指す"
	}
	if sub != "." {
		for _, seg := range strings.Split(sub, "/") {
			if skipDir(seg) {
				return fmt.Sprintf("走査しないディレクトリ %q を含む", seg)
			}
		}
	}
	if info, err := os.Stat(filepath.Join(target, "go.mod")); err != nil || !info.Mode().IsRegular() {
		return "go.mod を持つディレクトリではない"
	}
	return ""
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

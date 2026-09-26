package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
)

// sources は、inventory が見つけたファイルの中身 (1 回だけ読む。予算は tree が共有する)。
type sources struct {
	Go       map[string][]byte // .go (テストと、. や _ で始まる名前を含む)
	Markdown map[string][]byte // .md (referenceDir の下を含まない)
	Pkgs     []*pkgSource      // package の単位 (ディレクトリ)。名前順
}

// pkgSource は、1 つのディレクトリの、package を作る .go。
type pkgSource struct {
	Dir        string   // repo 相対 (例: tools/archtest)
	ImportPath string   // module path + module の中の相対 path
	Files      []string // package に含める .go の path (_test.go と、. か _ で始まる名前を除く。名前順)
}

// readSources は、inventory の全ファイルを読み、go.mod から package の単位を作る。
// package のファイルは、go tool が build に使うものと同じ規則で選ぶ (_test.go と、. か _ で始まる名前は含めない)。
// ビルドタグは見ない (全ファイルを 1 つの package として扱う)。
func (t *tree) readSources(inv *inventory) (*sources, error) {
	s := &sources{Go: map[string][]byte{}, Markdown: map[string][]byte{}}
	for _, p := range inv.Go {
		b, err := t.readFile(p)
		if err != nil {
			return nil, err
		}
		if err := checkSource(p, b); err != nil {
			return nil, err
		}
		if isPackageFile(p) { // 構文解析するものだけ数える (_test.go などは、読むだけ)
			if err := t.chargeGo(p, int64(len(b))); err != nil {
				return nil, err
			}
		}
		s.Go[p] = b
	}
	for _, p := range inv.Markdown {
		b, err := t.readFile(p)
		if err != nil {
			return nil, err
		}
		if err := checkSource(p, b); err != nil {
			return nil, err
		}
		s.Markdown[p] = b
	}
	modules := map[string]string{} // module のディレクトリ → module path
	for _, p := range inv.Mod {
		b, err := t.readFile(p)
		if err != nil {
			return nil, err
		}
		mod, err := parseModulePath(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		modules[path.Dir(p)] = mod
	}

	byDir := map[string][]string{}
	for _, p := range inv.Go {
		if !isPackageFile(p) {
			continue
		}
		byDir[path.Dir(p)] = append(byDir[path.Dir(p)], p)
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	for _, d := range dirs {
		if d == "." {
			return nil, fmt.Errorf("root 直下の .go は扱えない (出力の名前を作れない)")
		}
		modDir, ok := moduleOf(d, modules)
		if !ok {
			return nil, fmt.Errorf("%s: go.mod のあるディレクトリの外にある .go は扱えない", d)
		}
		imp := modules[modDir]
		if d != modDir {
			rel := d
			if modDir != "." {
				rel = strings.TrimPrefix(d, modDir+"/")
			}
			imp += "/" + rel
		}
		files := byDir[d]
		slices.Sort(files)
		s.Pkgs = append(s.Pkgs, &pkgSource{Dir: d, ImportPath: imp, Files: files})
	}
	return s, nil
}

// isPackageFile は、.go の path が、package を作る (構文解析する) ファイルか。go tool が build に使うものと同じ規則で、
// _test.go と、. か _ で始まる名前は含めない。
func isPackageFile(p string) bool {
	name := path.Base(p)
	return !strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// moduleOf は、ディレクトリ dir を含む、最も近い module のディレクトリを返す (go.mod のあるところ)。
func moduleOf(dir string, modules map[string]string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if _, ok := modules[d]; ok {
			return d, true
		}
		if d == "." || d == "/" {
			return "", false
		}
	}
}

// parseModulePath は、go.mod の module 行の module path を返す。最初の module ディレクティブだけを見る。
// 解釈は最小限 (module "path" と module path、行末の // のコメント)。block 形式や、不正な path は error にする。
func parseModulePath(data []byte) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "module" {
			continue
		}
		if len(f) != 2 {
			return "", fmt.Errorf("module 行を解釈できない")
		}
		mod := f[1]
		if mod[0] == '"' || mod[0] == '`' {
			u, err := strconv.Unquote(mod)
			if err != nil {
				return "", fmt.Errorf("module path を解釈できない: %w", err)
			}
			mod = u
		}
		if _, err := normalize(kindPath, mod); err != nil {
			return "", fmt.Errorf("module path が不正: %w", err)
		}
		return mod, nil
	}
	return "", fmt.Errorf("module 行が無い")
}

// parsedPkg は、構文解析した package。fset は、宣言の整形に使う (parse 時の FileSet でないと、1 行に潰れる)。
type parsedPkg struct {
	Src   *pkgSource
	Name  string // package 名
	Fset  *token.FileSet
	Files []*ast.File // src.Files と同じ順
}

// parsePackage は、package の .go を構文解析する (コメントも)。解析できないもの・別の package 名が混ざるものは error にする。
// go/doc は AST を書き換えるので、doc を作るものと、検査するものは、別々に parsePackage を呼ぶ。
func parsePackage(s *sources, p *pkgSource) (*parsedPkg, error) {
	pp := &parsedPkg{Src: p, Fset: token.NewFileSet()}
	for _, name := range p.Files {
		f, err := parser.ParseFile(pp.Fset, name, s.Go[name], parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s を構文解析できない: %w", name, rawParseError(s.Go[name], err))
		}
		if pp.Name == "" {
			pp.Name = f.Name.Name
		} else if f.Name.Name != pp.Name {
			return nil, fmt.Errorf("%s: package 名が %s と %s で食い違う (1 つのディレクトリに、複数の package)", p.Dir, pp.Name, f.Name.Name)
		}
		pp.Files = append(pp.Files, f)
	}
	return pp, nil
}

// lineOf は、pos の (実際の) 行番号。token.FileSet.Position は、//line ディレクティブで補正した行番号を返し、
// コメントの 1 行で、行数の上限・診断の行・複数行の値の判定を偽れる。補正しない PositionFor(pos, false) で求める。
// 行番号は、必ずこの関数で求める (Position は使わない。source_test.go の自己検査が見つける)。
func lineOf(fset *token.FileSet, pos token.Pos) int { return fset.PositionFor(pos, false).Line }

// rawParseError は、go/parser の構文の error を、実際の行で書き直す。parser の error の位置 (ファイル名と行) は、
// //line ディレクティブで補正されるので、そのままだと、存在しないファイルと行を指せる。バイトの位置 (Offset) は
// 補正されないので、そこから実際の行を数える。位置以外の error は、そのまま返す。
func rawParseError(src []byte, err error) error {
	var list scanner.ErrorList
	if !errors.As(err, &list) || len(list) == 0 {
		return err
	}
	first := list[0]
	line := 1 + bytes.Count(src[:min(max(first.Pos.Offset, 0), len(src))], []byte("\n"))
	msg := fmt.Sprintf("%d 行目: %s", line, first.Msg)
	if len(list) > 1 {
		msg += fmt.Sprintf(" (ほか %d 件)", len(list)-1)
	}
	return errors.New(msg)
}

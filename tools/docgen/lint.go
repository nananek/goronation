package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/doc/comment"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// rule は、lint の規則の ID。doc.go の「規則」の節と一致する (doc_test.go が確かめる)。
type rule string

const (
	rulePkgDocMissing  rule = "pkg-doc-missing"
	rulePkgDocMultiple rule = "pkg-doc-multiple"
	rulePkgDocStart    rule = "pkg-doc-start"
	rulePkgDocContract rule = "pkg-doc-contract"
	rulePkgDocHeading  rule = "pkg-doc-heading"
	rulePkgDocUsage    rule = "pkg-doc-usage"
	rulePkgDocRuleItem rule = "pkg-doc-rule-item"
	rulePkgDocRuleDup  rule = "pkg-doc-rule-dup"
	rulePkgDocLimits   rule = "pkg-doc-limits"
	rulePkgDocLength   rule = "pkg-doc-length"
	ruleExportedDoc    rule = "exported-doc"
	ruleMDPlace        rule = "md-place"
	ruleMDADRName      rule = "md-adr-name"
	ruleMDSize         rule = "md-size"
	ruleMDLink         rule = "md-link"
	ruleGenMissing     rule = "gen-missing"
	ruleGenStale       rule = "gen-stale"
	ruleGenExtra       rule = "gen-extra"
)

// allRules は、すべての規則 (doc.go の「規則」の節に、同じ集合が書かれている)。
var allRules = []rule{
	rulePkgDocMissing, rulePkgDocMultiple, rulePkgDocStart, rulePkgDocContract, rulePkgDocHeading, rulePkgDocUsage,
	rulePkgDocRuleItem, rulePkgDocRuleDup, rulePkgDocLimits, rulePkgDocLength, ruleExportedDoc,
	ruleMDPlace, ruleMDADRName, ruleMDSize, ruleMDLink, ruleGenMissing, ruleGenStale, ruleGenExtra,
}

// finding は、lint と生成物の照合で見つけた問題 1 件。
type finding struct {
	Path string // repo 相対
	Line int    // 0 なら、行は無い
	Rule rule
	Msg  string
}

func (f finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: [%s] %s", f.Path, f.Line, f.Rule, f.Msg)
	}
	return fmt.Sprintf("%s: [%s] %s", f.Path, f.Rule, f.Msg)
}

func sortFindings(fs []finding) {
	slices.SortFunc(fs, func(a, b finding) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		if c := strings.Compare(string(a.Rule), string(b.Rule)); c != 0 {
			return c
		}
		return strings.Compare(a.Msg, b.Msg)
	})
}

// docSections は、package doc の見出し (この順)。
var docSections = []string{"使い方", "規則", "方針", "限界", "関連"}

// ruleItemRE は、「規則」の節の箇条書きの先頭 (id: )。id は小文字とハイフンで、・ で複数書ける。
var ruleItemRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*(・[a-z][a-z0-9]*(-[a-z0-9]+)*)*: `)

// countLines は、行の数 (末尾が改行で終わらなくても、最後の行を数える)。
func countLines(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// lintPackage は、package の package doc と、exported の doc を検査する。go/doc は AST を書き換えるので、
// buildDoc とは別に、構文解析し直す。
func lintPackage(s *sources, p *pkgSource) ([]finding, error) {
	pp, err := parsePackage(s, p)
	if err != nil {
		return nil, err
	}
	var out []finding
	add := func(file string, line int, r rule, format string, args ...any) {
		out = append(out, finding{Path: file, Line: line, Rule: r, Msg: fmt.Sprintf(format, args...)})
	}

	var withDoc []int
	for i, f := range pp.Files {
		if f.Doc != nil {
			withDoc = append(withDoc, i)
		}
	}
	switch {
	case len(withDoc) == 0:
		add(p.Files[0], 0, rulePkgDocMissing, "package doc が無い (package 節の直前に、空行を挟まずに書いたコメントが、package doc になる)")
	case len(withDoc) > 1:
		names := make([]string, len(withDoc))
		for i, idx := range withDoc {
			names[i] = p.Files[idx]
		}
		add(names[0], 0, rulePkgDocMultiple, "package doc が複数のファイルにある: %s (1 か所にする。分けると、行数の上限を回避できる)", strings.Join(names, "・"))
	default:
		i := withDoc[0]
		lintPackageDoc(pp, i, p, add)
	}
	lintExported(pp, add)
	return out, nil
}

// lintPackageDoc は、package doc (pp.Files[i] にある 1 つ) の形式を検査する。
func lintPackageDoc(pp *parsedPkg, i int, p *pkgSource, add func(string, int, rule, string, ...any)) {
	file := p.Files[i]
	cg := pp.Files[i].Doc
	start := pp.Fset.Position(cg.Pos()).Line
	lines := pp.Fset.Position(cg.End()).Line - start + 1
	limit := pkgDocMaxLines
	enforcer := slices.Contains(enforcerPackages, p.Dir)
	if enforcer {
		limit = enforcerDocMaxLines
	}
	if lines > limit {
		add(file, start, rulePkgDocLength, "package doc が %d 行 (上限 %d 行。超えたら package を分ける)", lines, limit)
	}

	doc := new(comment.Parser).Parse(cg.Text())
	// 冒頭の 1 文。
	want := "Package " + pp.Name + " は"
	if pp.Name == "main" {
		want = "Command " + path.Base(p.Dir) + " は"
	}
	first, ok := comment.Block(nil), false
	if len(doc.Content) > 0 {
		first = doc.Content[0]
		_, ok = first.(*comment.Paragraph)
	}
	if !ok {
		add(file, start, rulePkgDocStart, "冒頭が段落ではない (「%s、〜。」の 1 文で始める)", want)
	} else {
		text := strings.Join(strings.Fields(flatten(first.(*comment.Paragraph).Text)), " ")
		switch {
		case !strings.HasPrefix(text, want):
			add(file, start, rulePkgDocStart, "冒頭が「%s」で始まらない", want)
		case !strings.HasSuffix(text, "。") || strings.Count(text, "。") != 1:
			add(file, start, rulePkgDocStart, "冒頭は、句点 (。) で終わる 1 文にする (一覧の概要になる)")
		}
	}
	// 契約の段落。
	if len(doc.Content) < 2 {
		add(file, start, rulePkgDocContract, "冒頭の 1 文の次に、契約の段落 (何を保証し、何を保証しないか) が無い")
	} else if _, ok := doc.Content[1].(*comment.Paragraph); !ok {
		add(file, start, rulePkgDocContract, "冒頭の 1 文の次が、契約の段落ではない")
	}

	// 見出しの順序と、節ごとの決まり。
	last, cur, usageLines := -1, "", 0
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, blk := range doc.Content {
		switch b := blk.(type) {
		case *comment.Heading:
			name := strings.TrimSpace(flatten(b.Text))
			idx := slices.Index(docSections, name)
			switch {
			case idx < 0:
				add(file, start, rulePkgDocHeading, "見出し「%s」は使えない (使える見出しと順序: %s)", name, strings.Join(docSections, "・"))
			case idx <= last:
				add(file, start, rulePkgDocHeading, "見出し「%s」の順序が違うか、重複している (順序: %s)", name, strings.Join(docSections, "・"))
			default:
				last = idx
			}
			cur = name
			seen[name] = true
		case *comment.Code:
			if cur == "使い方" {
				usageLines += countLines(strings.TrimRight(b.Text, "\n"))
			}
		case *comment.List:
			if cur != "規則" {
				continue
			}
			for _, item := range b.Items {
				text := ""
				if len(item.Content) > 0 {
					if para, ok := item.Content[0].(*comment.Paragraph); ok {
						text = strings.Join(strings.Fields(flatten(para.Text)), " ")
					}
				}
				m := ruleItemRE.FindString(text)
				if m == "" {
					add(file, start, rulePkgDocRuleItem, "「規則」の箇条書きが「id: 説明」の形ではない (id は小文字とハイフン。・ で複数): %.40s", text)
					continue
				}
				for _, id := range strings.Split(strings.TrimSuffix(m, ": "), "・") {
					if ids[id] {
						add(file, start, rulePkgDocRuleDup, "規則の id「%s」が重複している", id)
					}
					ids[id] = true
				}
			}
		}
	}
	if usageLines > usageMaxCodeLines {
		add(file, start, rulePkgDocUsage, "「使い方」のコードが %d 行 (合計 %d 行以内。最小の例にする)", usageLines, usageMaxCodeLines)
	}
	if enforcer && !seen["限界"] {
		add(file, start, rulePkgDocLimits, "検査・強制を担う package なのに、「限界」の節が無い (検出できないものと、代わりに守るものを書く)")
	}
}

// recvExported は、メソッドの受け取り側の型が exported か。
func recvExported(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return false
	}
	t := fd.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return ast.IsExported(x.Name)
		default:
			return false
		}
	}
}

// lintExported は、exported の型・関数・メソッド・変数・定数に、doc があり、「Name は」で始まることを検査する。
// var と const は、グループの doc も可 (その場合は、書き出しの形は問わない)。
func lintExported(pp *parsedPkg, add func(string, int, rule, string, ...any)) {
	for i, f := range pp.Files {
		file := pp.Src.Files[i]
		report := func(pos ast.Node, name string, cg *ast.CommentGroup, mustStart bool) {
			line := pp.Fset.Position(pos.Pos()).Line
			switch {
			case cg == nil:
				add(file, line, ruleExportedDoc, "exported の %s に doc が無い (「%s は〜。」で書く)", name, name)
			case mustStart && !strings.HasPrefix(cg.Text(), name+" は"):
				add(file, line, ruleExportedDoc, "%s の doc が「%s は」で始まらない", name, name)
			}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if ast.IsExported(d.Name.Name) && (d.Recv == nil || recvExported(d)) {
					report(d, d.Name.Name, d.Doc, true)
				}
			case *ast.GenDecl:
				single := !d.Lparen.IsValid() // 括弧の無い宣言 1 つ
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						if !ast.IsExported(sp.Name.Name) {
							continue
						}
						cg := sp.Doc
						if cg == nil && single {
							cg = d.Doc
						}
						report(sp, sp.Name.Name, cg, true)
					case *ast.ValueSpec:
						var names []string
						for _, n := range sp.Names {
							if ast.IsExported(n.Name) {
								names = append(names, n.Name)
							}
						}
						if len(names) == 0 {
							continue
						}
						cg, own := sp.Doc, true
						if cg == nil {
							cg, own = d.Doc, single
						}
						switch {
						case cg == nil:
							report(sp, names[0], nil, false)
						case own && !slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(cg.Text(), n+" は") }):
							add(file, pp.Fset.Position(sp.Pos()).Line, ruleExportedDoc, "%s の doc が「%s は」で始まらない", names[0], names[0])
						}
					}
				}
			}
		}
	}
}

// outsideGenerated は、ADR の README の、生成区間の中身を除いた全体 (marker の行は含む)。marker が不正なら、全体。
func outsideGenerated(readme string) string {
	lines := strings.SplitAfter(readme, "\n")
	begin, end := -1, -1
	for i, ln := range lines {
		switch strings.TrimSuffix(ln, "\n") {
		case adrBegin:
			begin = i
		case adrEnd:
			end = i
		}
	}
	if begin < 0 || end < begin {
		return readme
	}
	return strings.Join(lines[:begin+1], "") + strings.Join(lines[end:], "")
}

// mdLink は、Markdown の中のリンク先 1 つ (行は 1 始まり)。
type mdLink struct {
	Line   int
	Target string
}

var (
	inlineLinkRE = regexp.MustCompile(`!?\[[^\]\n]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"\n]*")?\s*\)`)
	linkDefRE    = regexp.MustCompile(`^ {0,3}\[[^\]\n]+\]:\s*<?([^\s>]+)>?`)
	escapedRE    = regexp.MustCompile("\\\\[!-/:-@\\[-`{-~]")
	fenceLineRE  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
)

// stripCodeSpans は、行の中の、行内のコード (同じ長さのバッククォートの囲み) を取り除く。閉じないものは、そのまま残す。
func stripCodeSpans(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		closed := -1
		for k := j; k < len(line); {
			if line[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-k == j-i {
				closed = m
				break
			}
			k = m
		}
		if closed < 0 {
			b.WriteString(line[i:j])
			i = j
			continue
		}
		i = closed
	}
	return b.String()
}

// markdownLinks は、Markdown のリンク先 (インラインと、参照の定義) を集める。コードブロックと、行内のコードは除く。
// 構文解析は簡易 (CommonMark の全体は解釈しない)。
func markdownLinks(md string) []mdLink {
	var out []mdLink
	fence := ""
	for i, line := range strings.Split(md, "\n") {
		if m := fenceLineRE.FindStringSubmatch(line); m != nil {
			switch {
			case fence == "":
				fence = m[1]
			case m[1][0] == fence[0] && len(m[1]) >= len(fence) && strings.Trim(strings.TrimSpace(line), m[1][:1]) == "":
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		line = escapedRE.ReplaceAllString(stripCodeSpans(line), "") // \[ など、リンクにならないもの
		for _, m := range inlineLinkRE.FindAllStringSubmatch(line, -1) {
			out = append(out, mdLink{Line: i + 1, Target: m[1]})
		}
		if m := linkDefRE.FindStringSubmatch(line); m != nil {
			out = append(out, mdLink{Line: i + 1, Target: m[1]})
		}
	}
	return out
}

var schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// lintLinks は、Markdown 1 つ (repo 相対の path) のリンクを検査する。http(s) と、# だけのリンクは見ない。
// 相対リンクは、repo の中の、実在するファイルかディレクトリでなければならない。
// exists は、repo 相対の path が実在するかを返す (生成される予定のファイルも、実在するとみなす)。
func lintLinks(from, md string, exists func(string) (bool, error)) ([]finding, error) {
	var out []finding
	bad := func(l mdLink, format string, args ...any) {
		out = append(out, finding{Path: from, Line: l.Line, Rule: ruleMDLink, Msg: fmt.Sprintf(format, args...)})
	}
	for _, l := range markdownLinks(md) {
		target := l.Target
		switch {
		case strings.HasPrefix(target, "#"):
			continue
		case schemeRE.MatchString(target):
			if s := strings.ToLower(target[:strings.Index(target, ":")]); s != "http" && s != "https" {
				bad(l, "リンク先 %.60s の scheme は使えない (使えるのは http・https・相対)", target)
			}
			continue
		case strings.HasPrefix(target, "//"):
			bad(l, "リンク先 %.60s は、host を指す (//) ので使えない", target)
			continue
		case strings.HasPrefix(target, "/"):
			bad(l, "リンク先 %.60s は、絶対 path なので使えない (相対で書く)", target)
			continue
		}
		rel := target
		if i := strings.IndexAny(rel, "#?"); i >= 0 {
			rel = rel[:i]
		}
		if rel == "" {
			continue
		}
		dec, err := url.PathUnescape(rel)
		if err != nil || !utf8.ValidString(dec) || strings.ContainsRune(dec, 0) {
			bad(l, "リンク先 %.60s を解釈できない", target)
			continue
		}
		p := path.Join(path.Dir(from), dec)
		if p == ".." || strings.HasPrefix(p, "../") {
			bad(l, "リンク先 %.60s が、repo の外を指す", target)
			continue
		}
		ok, err := exists(p)
		if err != nil {
			return nil, err
		}
		if !ok {
			bad(l, "リンク先 %.60s が無い (%s)", target, p)
		}
	}
	return out, nil
}

// lintMarkdown は、.md の配置・大きさ・リンクを検査する。res.Expected の生成物のリンクも検査する。
// 制御文字は、読んだ時点 (readSources) で error にしている。
func (t *tree) lintMarkdown(res *result) ([]finding, error) {
	var out []finding
	add := func(p string, r rule, format string, args ...any) {
		out = append(out, finding{Path: p, Rule: r, Msg: fmt.Sprintf(format, args...)})
	}
	size := func(p, text string, maxLines, maxChars int) {
		if n := countLines(text); n > maxLines {
			add(p, ruleMDSize, "%d 行 (上限 %d 行)", n, maxLines)
		}
		if n := utf8.RuneCountInString(text); n > maxChars {
			add(p, ruleMDSize, "%d 字 (上限 %d 字)", n, maxChars)
		}
	}
	for _, p := range sortedKeys(res.Src.Markdown) {
		text := string(res.Src.Markdown[p])
		base := path.Base(p)
		switch {
		case p == "README.md":
			size(p, text, readmeMaxLines, readmeMaxChars)
		case p == adrReadme:
			size(p, outsideGenerated(text), adrMaxLines, adrMaxChars)
		case path.Dir(p) == adrDir && adrFile.MatchString(base):
			size(p, text, adrMaxLines, adrMaxChars)
		case path.Dir(p) == adrDir:
			add(p, ruleMDADRName, "docs/adr の .md は、README.md か、NNNN-<英小文字と数字とハイフンのスラッグ>.md の名前にする")
		default:
			add(p, ruleMDPlace, ".md は、README.md・docs/adr/・docs/reference/ の外に置けない (上限の回避を防ぐ。文書は package doc に書く)")
		}
	}

	exists := func(p string) (bool, error) {
		if _, ok := res.Expected[p]; ok {
			return true, nil
		}
		for e := range res.Expected {
			if strings.HasPrefix(e, p+"/") {
				return true, nil
			}
		}
		if _, err := t.lstat(p); err != nil {
			if errors.Is(err, errBudget) {
				return false, err
			}
			return false, nil // 無い、または root の外を指すなど。リンク切れとして報告される
		}
		return true, nil
	}
	linted := map[string]string{}
	for p, b := range res.Src.Markdown {
		linted[p] = string(b)
	}
	for p, c := range res.Expected {
		linted[p] = c
	}
	for _, p := range sortedKeys(linted) {
		fs, err := lintLinks(p, linted[p], exists)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}

// lint は、repo 全体の lint (package doc・exported の doc・.md) の結果を、順序を決めて返す。
func (t *tree) lint(res *result) ([]finding, error) {
	var out []finding
	for _, p := range res.Src.Pkgs {
		if err := t.checkTime(); err != nil {
			return nil, err
		}
		fs, err := lintPackage(res.Src, p)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	md, err := t.lintMarkdown(res)
	if err != nil {
		return nil, err
	}
	out = append(out, md...)
	sortFindings(out)
	return out, nil
}

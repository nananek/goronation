package main

import (
	"slices"
	"strings"
	"testing"
)

// 攻撃者視点レビュー (2d63f6d) の finding 1: //line ディレクティブで、行の計算が偽れる。
//
// go/token の FileSet.Position は、//line ディレクティブで補正した行番号を返す (補正しない行は PositionFor(pos, false))。
// docgen は、行数の上限・診断の行番号・複数行の値の省略 (elideValues) を、補正した行番号で求める。
// コメントの中に "//line :N" を 1 行置くだけで、次の 3 つが偽れる。直すには、補正しない行番号で求める。

// package doc の行数の上限 (pkg-doc-length) を逃れる。200 行を超える package doc が、5 行に見える。
func TestPkgDocLengthIgnoresLineDirective(t *testing.T) {
	lines := append([]string{"Package core は、テスト。", "", "契約。"}, repeat("契約の続き。", 200)...)
	// docComment は "// " を付けるので、ディレクティブ ("//" と "line" の間に空白が無い) は、生の行で足す。
	doc := docComment(lines...) + "//line :5\n// 最後の行。\npackage core\n"
	got := lintFindings(t, map[string]string{"core/doc.go": doc})
	if !slices.Contains(got, "core/doc.go pkg-doc-length") {
		t.Errorf("200 行を超える package doc が、//line で pkg-doc-length を逃れた: %q", got)
	}
}

// 診断の行番号が、実際の行と違う (存在しない行を指す)。
func TestFindingLinesIgnoreLineDirective(t *testing.T) {
	// 実際の行: 1 が //line、2〜4 が package doc、5 が package、7 が type Bar。
	src := "//line :9999\n// Package core は、テスト。\n//\n// 契約。\npackage core\n\ntype Bar int\n"
	res, err := analyzeFiles(t, map[string]string{"core/doc.go": src})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.Findings {
		if f.Rule != ruleExportedDoc {
			continue
		}
		found = true
		if f.Line != 7 {
			t.Errorf("exported-doc の行が %d (実際の行は 7): //line で補正した行番号を出している", f.Line)
		}
	}
	if !found {
		t.Fatalf("exported-doc の指摘が無い (空振りで緑にしない): %v", res.Findings)
	}
}

// 複数行の値の省略 (elideValues) が効かなくなり、表の全体が生成物に出る。
func TestElideValuesIgnoresLineDirective(t *testing.T) {
	// 実際の行: 7 が var Table、11 が閉じの }。//line :7 で、閉じの } が 7 行目に見える (1 行の値になる)。
	src := docComment("Package core は、テスト。", "", "契約。") +
		"package core\n\n// Table は、表。\nvar Table = []string{\n\t\"a\",\n\t\"b\",\n//line :7\n}\n"
	res, err := analyzeFiles(t, map[string]string{"core/doc.go": src})
	if err != nil {
		t.Fatal(err)
	}
	page := res.Expected["docs/reference/core.md"]
	if !strings.Contains(page, "var Table = ...") || strings.Contains(page, `"a"`) {
		t.Errorf("複数行の値が、... に省かれず、生成物に出た (//line で 1 行に見せた):\n%s", page)
	}
}

// 構文の error の位置 (ファイル名と行) が、//line ディレクティブで偽れる (存在しないファイルと行を指す)。
func TestParseErrorIgnoresLineDirective(t *testing.T) {
	// 実際の行: 4 が構文の誤り。//line で、ファイルは fake.go の 100 行目に見える。
	src := "package core\n\n//line fake.go:100\nfunc {\n"
	_, err := analyzeFiles(t, map[string]string{"core/bad.go": src, "core/doc.go": goodDoc("core")})
	if err == nil {
		t.Fatal("error にすべき")
	}
	if !strings.Contains(err.Error(), "core/bad.go") || !strings.Contains(err.Error(), "4 行目") || strings.Contains(err.Error(), "fake.go") {
		t.Errorf("実際のファイルと行 (core/bad.go の 4 行目) を指すべき: %v", err)
	}
}

// lint の診断の行 (package doc の冒頭・グループの中の var) が、//line ディレクティブで偽れない。
func TestLintLinesIgnoreLineDirective(t *testing.T) {
	cases := []struct {
		name string
		src  string
		rule rule
		line int
	}{
		{"package doc の冒頭", "//line :9999\n\n// core は、テスト。\n//\n// 契約。\npackage core\n", rulePkgDocStart, 3},
		{"グループの中の var の doc", "// Package core は、テスト。\n//\n// 契約。\npackage core\n\n//line :9999\nvar (\n\t// 誤った書き出し。\n\tX int\n)\n", ruleExportedDoc, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := analyzeFiles(t, map[string]string{"core/doc.go": tc.src})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range res.Findings {
				if f.Rule == tc.rule {
					if f.Line != tc.line {
						t.Errorf("%s の行が %d (実際の行は %d): //line で補正した行番号を出している", tc.rule, f.Line, tc.line)
					}
					return
				}
			}
			t.Fatalf("%s の指摘が無い (空振りで緑にしない): %v", tc.rule, res.Findings)
		})
	}
}

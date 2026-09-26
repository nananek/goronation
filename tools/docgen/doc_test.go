package main

import (
	"fmt"
	"go/ast"
	"go/doc/comment"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// packageDoc は、doc.go の package doc (元の文字列。行数も見る) を返す。
func packageDoc(t *testing.T) (*ast.CommentGroup, *token.FileSet) {
	t.Helper()
	b, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doc.go", b, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if f.Doc == nil {
		t.Fatal("doc.go に package doc が無い")
	}
	return f.Doc, fset
}

// TestDocRulesMatchCode は、doc.go の「規則」の節の id の集合が、コードの規則 (allRules) と一致することを確認する
// (規則を足して、doc.go を直し忘れる、またはその逆を防ぐ。archtest の doc.go と同じ流儀)。
func TestDocRulesMatchCode(t *testing.T) {
	cg, _ := packageDoc(t)
	doc := new(comment.Parser).Parse(cg.Text())
	var ids []string
	inRules := false
	for _, blk := range doc.Content {
		switch b := blk.(type) {
		case *comment.Heading:
			inRules = strings.TrimSpace(flatten(b.Text)) == "規則"
		case *comment.List:
			if !inRules {
				continue
			}
			for _, item := range b.Items {
				text := strings.Join(strings.Fields(flatten(item.Content[0].(*comment.Paragraph).Text)), " ")
				m := ruleItemRE.FindString(text)
				if m == "" {
					t.Fatalf("規則の項目が「id: 説明」の形ではない: %q", text)
				}
				ids = append(ids, strings.Split(strings.TrimSuffix(m, ": "), "・")...)
			}
		}
	}
	var want []string
	for _, r := range allRules {
		want = append(want, string(r))
	}
	slices.Sort(ids)
	slices.Sort(want)
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("doc.go の規則の id と、コードの規則 (allRules) が違う:\n  doc  %v\n  code %v", ids, want)
	}
}

func comma(n int) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	return comma(n/1000) + "," + s[len(s)-3:]
}

// TestDocMentionsLimits は、doc.go に書いた上限の数値が、コードの定数と一致することを確認する
// (定数を変えて、doc.go の記述を直し忘れると、赤になる)。空白は、行の折り返しに依らないよう、除いて比べる。
func TestDocMentionsLimits(t *testing.T) {
	cg, _ := packageDoc(t)
	text := strings.Join(strings.Fields(cg.Text()), "")
	lim := defaultLimits
	want := []string{
		fmt.Sprintf("packagedocは%d行まで。検査・強制を担うpackageは%d行まで", pkgDocMaxLines, enforcerDocMaxLines),
		fmt.Sprintf("READMEは%d行・%s字、ADRは%d行・%s字まで", readmeMaxLines, comma(readmeMaxChars), adrMaxLines, comma(adrMaxChars)),
		fmt.Sprintf("使い方のコードは、合計%d行以内", usageMaxCodeLines),
		fmt.Sprintf("予算(%d秒・項目%s・ファイル%s・1ファイル1MiB・合計%dMiB・深さ%d)",
			int(lim.Timeout.Seconds()), comma(lim.MaxEntries), comma(lim.MaxFiles), lim.MaxTotalBytes>>20, lim.MaxDepth),
		fmt.Sprintf("%d秒の猶予", int(watchdogGrace.Seconds())),
	}
	if lim.MaxFileBytes != 1<<20 {
		t.Fatalf("MaxFileBytes = %d: doc.go の「1 ファイル 1 MiB」を直す", lim.MaxFileBytes)
	}
	for _, w := range want {
		if !strings.Contains(text, strings.ReplaceAll(w, " ", "")) {
			t.Errorf("doc.go に %q に当たる記述が無い (定数と食い違っている)", w)
		}
	}
}

// TestDocListsEnforcers は、doc.go の記述 (enforcerPackages) と、一覧に docgen 自身が含まれることを確認する。
func TestDocListsEnforcers(t *testing.T) {
	for _, p := range []string{"tools/archtest", "tools/docgen"} {
		if !slices.Contains(enforcerPackages, p) {
			t.Errorf("enforcerPackages に %s が無い", p)
		}
	}
}

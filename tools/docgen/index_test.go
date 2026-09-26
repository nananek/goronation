package main

import (
	"reflect"
	"strings"
	"testing"
)

func adrSources(files map[string]string) *sources {
	s := &sources{Markdown: map[string][]byte{}}
	for p, c := range files {
		s.Markdown[p] = []byte(c)
	}
	return s
}

func TestLoadADRs(t *testing.T) {
	s := adrSources(map[string]string{
		"docs/adr/0000-template.md":   "何が書いてあっても、0000 は読まない (# NNNN. タイトル)\x00",
		"docs/adr/0002-second.md":     "# 0002. 二つ目\n\n- 状態: 置き換えられた (→ 0003)\n- 日付: x\n",
		"docs/adr/0001-first.md":      "# 0001. 一つ目\n\n- 日付: 2026-01-01\n- 状態:   採用  \n\n## 状況\n\n- 状態: 本文中の行は、見ない\n",
		"docs/adr/README.md":          "ADR ではない\n",
		"docs/adr/notes.md":           "ADR の名前ではない\n",
		"docs/adr/0004_under.md":      "名前の形が違う\n",
		"docs/adr/0005-Upper.md":      "名前の形が違う\n",
		"docs/adr/sub/0009-deep.md":   "# 0009. 別のディレクトリ\n- 状態: 採用\n",
		"README.md":                   "ADR ではない\n",
		"docs/adr/0003-third-one.md":  "# 0003. 三つ目\n- 状態: 提案\n",
		"docs/adr/00010-toolong.md":   "名前の形が違う\n",
		"docs/adr/0006-a1-b2-c3.md":   "# 0006. 数字を含むスラッグ\n- 状態: 採用\n",
		"docs/adr/0007-a--b.md":       "連続するハイフンは、名前の形が違う\n",
		"docs/adr/0008-trailing-.md":  "末尾のハイフンは、名前の形が違う\n",
		"other/docs/adr/0001-x.md":    "# 0001. 別の場所\n- 状態: 採用\n",
		"docs/adr/0001-first.md.orig": "ADR ではない\n",
	})
	got, err := loadADRs(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []adr{
		{"0000", "0000-template.md", "テンプレート", "-"},
		{"0001", "0001-first.md", "一つ目", "採用"},
		{"0002", "0002-second.md", "二つ目", "置き換えられた (→ 0003)"},
		{"0003", "0003-third-one.md", "三つ目", "提案"},
		{"0006", "0006-a1-b2-c3.md", "数字を含むスラッグ", "採用"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadADRs =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestLoadADRsErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"1 行目が見出しではない": {"docs/adr/0001-a.md": "タイトル\n- 状態: 採用\n"},
		"1 行目が # だけ":   {"docs/adr/0001-a.md": "#\n- 状態: 採用\n"},
		"番号の桁が違う":      {"docs/adr/0001-a.md": "# 001. x\n- 状態: 採用\n"},
		"番号がファイル名と違う":  {"docs/adr/0001-a.md": "# 0002. x\n- 状態: 採用\n"},
		"タイトルが空":       {"docs/adr/0001-a.md": "# 0001. \n- 状態: 採用\n"},
		"状態が無い":        {"docs/adr/0001-a.md": "# 0001. x\n\n- 日付: 2026-01-01\n"},
		"状態が最初の節の後にだけ": {"docs/adr/0001-a.md": "# 0001. x\n\n## 状況\n\n- 状態: 採用\n"},
		"状態が空":         {"docs/adr/0001-a.md": "# 0001. x\n- 状態:\n"},
		"空のファイル":       {"docs/adr/0001-a.md": ""},
		"番号が重複":        {"docs/adr/0001-a.md": "# 0001. a\n- 状態: 採用\n", "docs/adr/0001-b.md": "# 0001. b\n- 状態: 採用\n"},
		"0000 が重複":     {"docs/adr/0000-a.md": "x", "docs/adr/0000-b.md": "y"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			adrs, err := loadADRs(adrSources(files))
			if err == nil {
				t.Errorf("error にすべき: %+v", adrs)
			}
		})
	}
}

func TestRenderADRIndex(t *testing.T) {
	got, err := renderADRIndex([]adr{
		{"0000", "0000-template.md", "テンプレート", "-"},
		{"0001", "0001-a.md", "表 | 記号 * _ `x` <b> [y](javascript:alert(1)) & # $ ~", "採用 | 条件付き"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "| 番号 | タイトル | 状態 |\n|---|---|---|\n" +
		"| [0000](0000-template.md) | テンプレート | - |\n" +
		"| [0001](0001-a.md) | 表 \\| 記号 \\* \\_ \\`x\\` \\<b\\> \\[y\\](javascript:alert(1)) \\& \\# \\$ \\~ | 採用 \\| 条件付き |\n"
	if got != want {
		t.Errorf("renderADRIndex =\n%s\nwant\n%s", got, want)
	}

	for name, a := range map[string]adr{
		"題に ESC":        {"0001", "0001-a.md", "a\x1bb", "採用"},
		"状態に BEL":       {"0001", "0001-a.md", "a", "b\x07"},
		"題に RLO":        {"0001", "0001-a.md", "a\u202eb", "採用"},
		"ファイル名に空白":      {"0001", "0001 a.md", "a", "採用"},
		"ファイル名に括弧":      {"0001", "0001-a).md", "a", "採用"},
		"番号に記号":         {"00`1", "0001-a.md", "a", "採用"},
		"ファイル名が ..":     {"0001", "..", "a", "採用"},
		"ファイル名に scheme": {"0001", "javascript:x", "a", "採用"},
	} {
		if out, err := renderADRIndex([]adr{a}); err == nil {
			t.Errorf("%s: error にすべき:\n%s", name, out)
		}
	}
}

func TestSpliceADRIndex(t *testing.T) {
	const table = "| 番号 |\n|---|\n| 行 |\n"
	b, e := adrBegin, adrEnd

	t.Run("区間だけを置き換え、外は 1 バイトも変えない", func(t *testing.T) {
		prefix := "# 見出し\n\n前の文。  末尾の空白\t\n\n"
		suffix := "\n後ろの文。" + b + " は、行の途中なので marker ではない\n\n" + "```\n" + "コード\n" + "```\n"
		in := prefix + b + "\n古い 1\n古い 2\n" + e + suffix
		got, err := spliceADRIndex(in, table)
		if err != nil {
			t.Fatal(err)
		}
		if want := prefix + b + "\n" + table + e + suffix; got != want {
			t.Errorf("got:\n%q\nwant:\n%q", got, want)
		}
		// 冪等。
		if again, err := spliceADRIndex(got, table); err != nil || again != got {
			t.Errorf("2 回目で変わった: %q, %v", again, err)
		}
	})

	t.Run("空の区間 (枠だけ) にも入る", func(t *testing.T) {
		got, err := spliceADRIndex(b+"\n"+e+"\n", table)
		if err != nil || got != b+"\n"+table+e+"\n" {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("末尾に改行が無くても入る", func(t *testing.T) {
		got, err := spliceADRIndex("x\n"+b+"\n"+e, table)
		if err != nil || got != "x\n"+b+"\n"+table+e {
			t.Errorf("got %q, %v", got, err)
		}
	})

	bad := map[string]string{
		"marker が無い":    "x\n",
		"空":             "",
		"begin だけ":      "x\n" + b + "\n",
		"end だけ":        "x\n" + e + "\n",
		"begin が重複":     b + "\n" + b + "\n" + e + "\n",
		"end が重複":       b + "\n" + e + "\n" + e + "\n",
		"順序が逆":          e + "\n" + b + "\n",
		"2 つの区間":        b + "\n" + e + "\n" + b + "\n" + e + "\n",
		"marker の前後に空白": " " + b + "\n" + e + "\n",
		"marker の後ろに空白": b + " \n" + e + "\n",
		"CRLF の marker": b + "\r\n" + e + "\r\n",
		"marker の途中に文字": "x" + b + "\n" + e + "\n",
		"marker の綴りが違う": "<!-- docgen:adr-index start -->\n" + e + "\n",
		"marker がコードブロックの中にもある": b + "\n" + e + "\n```\n" + b + "\n```\n",
	}
	for name, in := range bad {
		if got, err := spliceADRIndex(in, table); err == nil {
			t.Errorf("%s: error にすべき: %q", name, got)
		}
	}
}

// TestADRIndexInAnalyze は、analyze が、ADR の追加・番号の重複・marker の欠落を、error にすることを確認する。
func TestADRIndexInAnalyze(t *testing.T) {
	doc := "// Package core は、テスト。\npackage core\n"
	t.Run("ADR を足すと、索引に行が出る", func(t *testing.T) {
		res, err := analyzeFiles(t, map[string]string{
			"core/doc.go":        doc,
			"docs/adr/0001-a.md": "# 0001. 最初\n- 状態: 採用\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Expected[adrReadme]; !strings.Contains(got, "| [0001](0001-a.md) | 最初 | 採用 |") {
			t.Errorf("索引に行が無い:\n%s", got)
		}
	})
	for name, files := range map[string]map[string]string{
		"CRLF の ADR":   {"docs/adr/0001-a.md": "# 0001. x\r\n- 状態: 採用\r\n"},
		"番号の重複":        {"docs/adr/0001-a.md": "# 0001. a\n- 状態: 採用\n", "docs/adr/0001-b.md": "# 0001. b\n- 状態: 採用\n"},
		"読めない ADR":     {"docs/adr/0001-a.md": "壊れている\n"},
		"marker の欠落":   {"docs/adr/README.md": "# ADR\n"},
		"marker の重複":   {"docs/adr/README.md": adrBegin + "\n" + adrBegin + "\n" + adrEnd + "\n"},
		"marker の順序が逆": {"docs/adr/README.md": adrEnd + "\n" + adrBegin + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			files["core/doc.go"] = doc
			if _, err := analyzeFiles(t, files); err == nil {
				t.Error("error にすべき")
			}
		})
	}
}

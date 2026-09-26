package main

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// lintFindings は、files (scaffold を足したもの) の repo を analyze し、lint の結果を「path 規則」の列にして返す。
func lintFindings(t *testing.T, files map[string]string) []string {
	t.Helper()
	res, err := analyzeFiles(t, files)
	if err != nil {
		t.Fatal(err)
	}
	return keysOf(res.Findings)
}

func keysOf(fs []finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Path+" "+string(f.Rule))
	}
	slices.Sort(out)
	return out
}

func expectRules(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lint の結果:\n  got  %q\n  want %q", got, want)
	}
}

// goodDoc は、lint に通る package doc (パッケージ名 name)。
func goodDoc(name string, extra ...string) string {
	lines := []string{
		"Package " + name + " は、テスト用の package。", "",
		"何を保証し、何を保証しないかを書く、契約の段落。", "",
		"# 使い方", "", "\tx := " + name + ".New()", "",
		"# 規則", "", "  - alpha: 1 項目目。", "  - beta・gamma-x: 2 項目目。", "",
		"# 方針", "", "方針の段落。", "",
		"# 限界", "", "  - 検出できないもの。", "",
		"# 関連", "", "docs/adr/0003-documentation-policy.md",
	}
	lines = append(lines, extra...)
	return docComment(lines...) + "package " + name + "\n"
}

// docWithLines は、ちょうど n 行 (空行を含む) の package doc を作る。
func docWithLines(name string, n int) string {
	lines := []string{"Package " + name + " は、テスト。", "", "契約の段落。"}
	for len(lines) < n {
		lines = append(lines, "契約の続き。")
	}
	return docComment(lines...) + "package " + name + "\n"
}

func TestLintGood(t *testing.T) {
	expectRules(t, lintFindings(t, map[string]string{"core/doc.go": goodDoc("core")}))
}

func TestLintPackageDoc(t *testing.T) {
	const doc = "core/doc.go"
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"package doc が無い", map[string]string{doc: "package core\n"}, []string{doc + " pkg-doc-missing"}},
		{"空行があると、doc にならない", map[string]string{doc: "// Package core は、テスト。\n//\n// 契約。\n\npackage core\n"}, []string{doc + " pkg-doc-missing"}},
		{"複数のファイルにある", map[string]string{doc: goodDoc("core"), "core/other.go": docComment("Package core は、別の doc。", "", "契約。") + "package core\n"}, []string{doc + " pkg-doc-multiple"}},
		{"名前が違う", map[string]string{doc: docComment("Package other は、テスト。", "", "契約。") + "package core\n"}, []string{doc + " pkg-doc-start"}},
		{"Package で始まらない", map[string]string{doc: docComment("core は、テスト。", "", "契約。") + "package core\n"}, []string{doc + " pkg-doc-start"}},
		{"「は」が無い", map[string]string{doc: docComment("Package core を置く。", "", "契約。") + "package core\n"}, []string{doc + " pkg-doc-start"}},
		{"句点で終わらない", map[string]string{doc: docComment("Package core は、テスト", "", "契約。") + "package core\n"}, []string{doc + " pkg-doc-start"}},
		{"2 文", map[string]string{doc: docComment("Package core は、テスト。もう 1 文。", "", "契約。") + "package core\n"}, []string{doc + " pkg-doc-start"}},
		{"見出しで始まる", map[string]string{doc: docComment("# 使い方", "", "契約。") + "package core\n"}, []string{doc + " pkg-doc-start"}},
		{"1 文だけで、契約が無い", map[string]string{doc: docComment("Package core は、テスト。") + "package core\n"}, []string{doc + " pkg-doc-contract"}},
		{"契約の代わりに見出し", map[string]string{doc: docComment("Package core は、テスト。", "", "# 使い方", "", "説明。") + "package core\n"}, []string{doc + " pkg-doc-contract"}},
		{"契約の代わりにコード", map[string]string{doc: docComment("Package core は、テスト。", "", "\tcode") + "package core\n"}, []string{doc + " pkg-doc-contract"}},
		{"main は Command", map[string]string{"cmd/go.mod": "module example.com/m/cmd\n", "core/doc.go": goodDoc("core"), "cmd/tool/main.go": docComment("Command tool は、テスト。", "", "契約。") + "package main\n"}, nil},
		{"main が Package で始まる", map[string]string{"cmd/go.mod": "module example.com/m/cmd\n", "core/doc.go": goodDoc("core"), "cmd/tool/main.go": docComment("Package main は、テスト。", "", "契約。") + "package main\n"}, []string{"cmd/tool/main.go pkg-doc-start"}},
		{"main の Command の名前が、ディレクトリ名と違う", map[string]string{"cmd/go.mod": "module example.com/m/cmd\n", "core/doc.go": goodDoc("core"), "cmd/tool/main.go": docComment("Command main は、テスト。", "", "契約。") + "package main\n"}, []string{"cmd/tool/main.go pkg-doc-start"}},
		{"未定義の見出し", map[string]string{doc: goodDoc("core", "", "# おまけ", "", "本文。")}, []string{doc + " pkg-doc-heading"}},
		{"見出しの順序が違う", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - a: 1。", "", "# 使い方", "", "\tx") + "package core\n"}, []string{doc + " pkg-doc-heading"}},
		{"見出しが重複", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 方針", "", "1。", "", "# 方針", "", "2。") + "package core\n"}, []string{doc + " pkg-doc-heading"}},
		{"使い方のコードが 6 行", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 使い方", "", "\t1", "\t2", "\t3", "\t4", "\t5", "\t6") + "package core\n"}, []string{doc + " pkg-doc-usage"}},
		{"使い方のコードが 5 行 (境界)", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 使い方", "", "\t1", "\t2", "\t3", "\t4", "\t5") + "package core\n"}, nil},
		{"使い方のコードが 2 つで合計 6 行", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 使い方", "", "\t1", "\t2", "\t3", "", "間。", "", "\t4", "\t5", "\t6") + "package core\n"}, []string{doc + " pkg-doc-usage"}},
		{"使い方以外のコードは、行数を問わない", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 方針", "", "\t1", "\t2", "\t3", "\t4", "\t5", "\t6", "\t7") + "package core\n"}, nil},
		{"規則の項目に id が無い", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - 説明だけの項目。") + "package core\n"}, []string{doc + " pkg-doc-rule-item"}},
		{"規則の id が大文字", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - Alpha: 説明。") + "package core\n"}, []string{doc + " pkg-doc-rule-item"}},
		{"規則の id に _", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - a_b: 説明。") + "package core\n"}, []string{doc + " pkg-doc-rule-item"}},
		{"規則の id の後に空白が無い", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - a:説明。") + "package core\n"}, []string{doc + " pkg-doc-rule-item"}},
		{"規則の id の末尾が -", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - a-: 説明。") + "package core\n"}, []string{doc + " pkg-doc-rule-item"}},
		{"規則の節の段落は、項目ではないので見ない", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "説明の段落: これは項目ではない。", "", "  - a: 1。") + "package core\n"}, nil},
		{"規則以外の節の箇条書きは、見ない", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 方針", "", "  - 説明だけの項目。") + "package core\n"}, nil},
		{"規則の id が重複 (項目をまたぐ)", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - a: 1。", "  - b: 2。", "  - a: 3。") + "package core\n"}, []string{doc + " pkg-doc-rule-dup"}},
		{"規則の id が重複 (・ の中)", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - a・a: 1。") + "package core\n"}, []string{doc + " pkg-doc-rule-dup"}},
		{"規則の番号付きの項目も見る", map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", " 1. 番号付きで、id が無い。") + "package core\n"}, []string{doc + " pkg-doc-rule-item"}},
		{"一般の package は 30 行まで (境界)", map[string]string{doc: docWithLines("core", 30)}, nil},
		{"一般の package が 31 行", map[string]string{doc: docWithLines("core", 31)}, []string{doc + " pkg-doc-length"}},
		{"限界の節を持っても、一般の package は 30 行まで", map[string]string{doc: docComment(append([]string{"Package core は、テスト。", "", "契約。", "", "# 限界", ""}, repeat("限界の続き。", 25)...)...) + "package core\n"}, []string{doc + " pkg-doc-length"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"core/doc.go": goodDoc("core")}
			for k, v := range tc.files {
				files[k] = v
			}
			expectRules(t, lintFindings(t, files), tc.want...)
		})
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// TestLintEnforcers は、検査・強制を担う package (enforcerPackages) が、限界の節を必須とし、
// package doc を 120 行まで書けることを確認する。
func TestLintEnforcers(t *testing.T) {
	mod := map[string]string{"tools/go.mod": "module example.com/m/tools\n", "core/doc.go": goodDoc("core")}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range mod {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	t.Run("限界が無い", func(t *testing.T) {
		got := lintFindings(t, with(map[string]string{"tools/archtest/doc.go": docComment("Package archtest は、テスト。", "", "契約。") + "package archtest\n"}))
		expectRules(t, got, "tools/archtest/doc.go pkg-doc-limits")
	})
	t.Run("限界がある", func(t *testing.T) {
		got := lintFindings(t, with(map[string]string{"tools/archtest/doc.go": docComment("Package archtest は、テスト。", "", "契約。", "", "# 限界", "", "  - 検出できないもの。") + "package archtest\n"}))
		expectRules(t, got)
	})
	t.Run("同じ名前の一般の package は、限界が要らない", func(t *testing.T) {
		got := lintFindings(t, with(map[string]string{"tools/other/doc.go": docComment("Package other は、テスト。", "", "契約。") + "package other\n"}))
		expectRules(t, got)
	})
	long := func(n int) string {
		lines := []string{"Package archtest は、テスト。", "", "契約。", "", "# 限界", ""}
		return docComment(append(lines, repeat("限界の続き。", n-len(lines))...)...) + "package archtest\n"
	}
	t.Run("120 行まで (境界)", func(t *testing.T) {
		expectRules(t, lintFindings(t, with(map[string]string{"tools/archtest/doc.go": long(120)})))
	})
	t.Run("121 行", func(t *testing.T) {
		expectRules(t, lintFindings(t, with(map[string]string{"tools/archtest/doc.go": long(121)})), "tools/archtest/doc.go pkg-doc-length")
	})
}

func TestLintExportedDoc(t *testing.T) {
	const x = "core/x.go"
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"func に doc が無い", "package core\n\nfunc F() {}\n", []string{x + " exported-doc"}},
		{"type に doc が無い", "package core\n\ntype T struct{}\n", []string{x + " exported-doc"}},
		{"var に doc が無い", "package core\n\nvar V = 1\n", []string{x + " exported-doc"}},
		{"const に doc が無い", "package core\n\nconst C = 1\n", []string{x + " exported-doc"}},
		{"method に doc が無い", "package core\n\n// T は、型。\ntype T struct{}\n\nfunc (T) M() {}\n", []string{x + " exported-doc"}},
		{"ポインタ受け取りの method に doc が無い", "package core\n\n// T は、型。\ntype T struct{}\n\nfunc (*T) M() {}\n", []string{x + " exported-doc"}},
		{"generics の method に doc が無い", "package core\n\n// T は、型。\ntype T[P any] struct{}\n\nfunc (T[P]) M() {}\n", []string{x + " exported-doc"}},
		{"「Name は」で始まらない (func)", "package core\n\n// 関数。\nfunc F() {}\n", []string{x + " exported-doc"}},
		{"「Name は」で始まらない (別の名前)", "package core\n\n// G は、関数。\nfunc F() {}\n", []string{x + " exported-doc"}},
		{"「Name は」で始まらない (は が無い)", "package core\n\n// F を返す。\nfunc F() {}\n", []string{x + " exported-doc"}},
		{"「Name は」で始まらない (type)", "package core\n\n// 型。\ntype T struct{}\n", []string{x + " exported-doc"}},
		{"適合", "package core\n\n// F は、関数。\nfunc F() {}\n\n// T は、型。\ntype T struct{}\n\n// M は、メソッド。\nfunc (T) M() {}\n\n// V は、変数。\nvar V = 1\n\n// C は、定数。\nconst C = 1\n", nil},
		{"unexported は要らない", "package core\n\nfunc f() {}\n\ntype t struct{}\n\nvar v = 1\n\nconst c = 1\n\nfunc (t) M() {}\n\nfunc (*t) N() {}\n", nil},
		{"unexported の型の exported method は要らない", "package core\n\ntype t struct{}\n\nfunc (t) Exported() {}\n", nil},
		{"グループの doc で足りる (var・const)", "package core\n\n// 定数の群。\nconst (\n\tA = 1\n\tB = 2\n)\n\n// 変数の群。\nvar (\n\tV1 = 1\n\tV2 = 2\n)\n", nil},
		{"グループの doc も、個別の doc も無い", "package core\n\nconst (\n\tA = 1\n)\n", []string{x + " exported-doc"}},
		{"グループの中の個別の doc は、Name で始まる", "package core\n\n// 群。\nconst (\n\t// A は、定数。\n\tA = 1\n\t// 定数。\n\tB = 2\n)\n", []string{x + " exported-doc"}},
		{"複数の名前の doc は、どれかの名前で始まればよい", "package core\n\n// B は、B と A の定数。\nvar A, B = 1, 2\n", nil},
		{"複数の名前の doc が、どの名前でも始まらない", "package core\n\n// C は、定数。\nvar A, B = 1, 2\n", []string{x + " exported-doc"}},
		{"グループの type は、個別の doc が要る", "package core\n\n// 群。\ntype (\n\tA struct{}\n)\n", []string{x + " exported-doc"}},
		{"グループの type の個別の doc", "package core\n\ntype (\n\t// A は、型。\n\tA struct{}\n)\n", nil},
		{"blank の var は要らない", "package core\n\nvar _ = 1\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectRules(t, lintFindings(t, map[string]string{"core/doc.go": goodDoc("core"), x: tc.src}), tc.want...)
		})
	}
}

// TestLintExportedIgnoresTests は、_test.go と、. か _ で始まる名前のファイルの exported を、要求しないことを確認する
// (go tool は、build に使わない)。
func TestLintExportedIgnoresTests(t *testing.T) {
	expectRules(t, lintFindings(t, map[string]string{
		"core/doc.go":     goodDoc("core"),
		"core/x_test.go":  "package core\n\nfunc Exported() {}\n",
		"core/_skip.go":   "package core\n\nfunc Skipped() {}\n",
		"core/.hidden.go": "package core\n\nfunc Hidden() {}\n",
		"core/y_test.go":  "package core_test\n\nfunc Ext() {}\n",
	}))
}

func TestLintMarkdown(t *testing.T) {
	lines := func(n int) string { return strings.Repeat("行\n", n) }
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"README は 30 行 (境界)", map[string]string{"README.md": lines(30)}, nil},
		{"README が 31 行", map[string]string{"README.md": lines(31)}, []string{"README.md md-size"}},
		{"README が 2,000 字 (境界)", map[string]string{"README.md": strings.Repeat("あ", 1999) + "\n"}, nil},
		{"README が 2,001 字", map[string]string{"README.md": strings.Repeat("あ", 2000) + "\n"}, []string{"README.md md-size"}},
		{"README の末尾に改行が無くても数える", map[string]string{"README.md": strings.TrimSuffix(lines(31), "\n")}, []string{"README.md md-size"}},
		{"README が 31 行かつ 2,001 字は、2 件", map[string]string{"README.md": strings.Repeat("あ\n", 1001)}, []string{"README.md md-size", "README.md md-size"}},
		{"ADR が 60 行 (境界)", map[string]string{"docs/adr/0001-a.md": "# 0001. a\n- 状態: 採用\n" + lines(58)}, nil},
		{"ADR が 61 行", map[string]string{"docs/adr/0001-a.md": "# 0001. a\n- 状態: 採用\n" + lines(59)}, []string{"docs/adr/0001-a.md md-size"}},
		{"ADR が 4,000 字 (境界)", map[string]string{"docs/adr/0001-a.md": "# 0001. a\n- 状態: 採用\n" + strings.Repeat("あ", 3980) + "\n"}, nil},
		{"ADR が 4,001 字", map[string]string{"docs/adr/0001-a.md": "# 0001. a\n- 状態: 採用\n" + strings.Repeat("あ", 3981) + "\n"}, []string{"docs/adr/0001-a.md md-size"}},
		{"ADR の README は、生成区間の外だけを数える", map[string]string{"docs/adr/README.md": "# ADR\n" + adrBegin + "\n" + lines(200) + adrEnd + "\n"}, nil},
		{"ADR の README の、区間の外が 60 行を超える", map[string]string{"docs/adr/README.md": "# ADR\n" + adrBegin + "\n" + adrEnd + "\n" + lines(60)}, []string{"docs/adr/README.md md-size"}},
		{"別の場所の .md", map[string]string{"docs/other.md": "x\n"}, []string{"docs/other.md md-place"}},
		{"package の README", map[string]string{"core/README.md": "x\n"}, []string{"core/README.md md-place"}},
		{"root 直下の別名", map[string]string{"NOTES.md": "x\n", "readme.md": "x\n"}, []string{"NOTES.md md-place", "readme.md md-place"}},
		{"大文字の拡張子", map[string]string{"docs/X.MD": "x\n"}, []string{"docs/X.MD md-place"}},
		{".markdown", map[string]string{"docs/x.markdown": "x\n"}, []string{"docs/x.markdown md-place"}},
		{"docs/adr の下のディレクトリ", map[string]string{"docs/adr/sub/0009-a.md": "# 0009. a\n- 状態: 採用\n"}, []string{"docs/adr/sub/0009-a.md md-place"}},
		{"docs/adr の ADR ではない名前", map[string]string{"docs/adr/notes.md": "x\n", "docs/adr/0002_x.md": "x\n", "docs/adr/0003-X.md": "x\n"}, []string{"docs/adr/notes.md md-adr-name", "docs/adr/0002_x.md md-adr-name", "docs/adr/0003-X.md md-adr-name"}},
		{"docs/reference の外の .md は、生成物ではない", map[string]string{"docs/referenceX/a.md": "x\n"}, []string{"docs/referenceX/a.md md-place"}},
		{"辿らないディレクトリの .md は、見ない (限界)", map[string]string{"testdata/a.md": "x\n", ".github/a.md": "x\n", "vendor/a.md": "x\n"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"core/doc.go": goodDoc("core")}
			for k, v := range tc.files {
				files[k] = v
			}
			expectRules(t, lintFindings(t, files), tc.want...)
		})
	}
}

func TestLintLinks(t *testing.T) {
	const readme = "README.md"
	cases := []struct {
		name string
		md   string
		want []string
	}{
		{"実在する相対リンク", "[a](docs/adr/0001-a.md) [b](core/doc.go) [c](core) [d](core/)\n", nil},
		{"アンカー付き", "[a](docs/adr/0001-a.md#section) [b](#anchor) [c](docs/adr/0001-a.md?x=1#y)\n", nil},
		{"外部 URL は見ない", "[a](https://example.com/none) [b](http://example.com/none)\n", nil},
		{"リンク切れ", "[a](docs/none.md)\n", []string{readme + " md-link"}},
		{"リンク切れ (アンカー付き)", "[a](docs/none.md#x)\n", []string{readme + " md-link"}},
		{"repo の外", "[a](../outside.md)\n", []string{readme + " md-link"}},
		{"repo の外 (深い)", "[a](docs/../../outside.md)\n", []string{readme + " md-link"}},
		{"絶対 path", "[a](/etc/passwd)\n", []string{readme + " md-link"}},
		{"scheme-relative", "[a](//evil.example/x)\n", []string{readme + " md-link"}},
		{"javascript:", "[a](javascript:alert(1))\n", []string{readme + " md-link"}},
		{"大文字の JavaScript:", "[a](JaVaScRiPt:x)\n", []string{readme + " md-link"}},
		{"data:", "[a](data:text/html,x)\n", []string{readme + " md-link"}},
		{"file:", "[a](file:///etc/passwd)\n", []string{readme + " md-link"}},
		{"mailto:", "[a](mailto:a@example.com)\n", []string{readme + " md-link"}},
		{"画像も見る", "![a](docs/none.png)\n", []string{readme + " md-link"}},
		{"参照の定義", "[a]: docs/none.md\n", []string{readme + " md-link"}},
		{"参照の定義 (実在)", "[a]: docs/adr/0001-a.md\n", nil},
		{"参照の定義 (外部)", "[a]: https://example.com\n", nil},
		{"title 付き", "[a](docs/adr/0001-a.md \"題\")\n", nil},
		{"title 付きのリンク切れ", "[a](docs/none.md \"題\")\n", []string{readme + " md-link"}},
		{"山括弧", "[a](<docs/adr/0001-a.md>)\n", nil},
		{"山括弧のリンク切れ", "[a](<docs/none.md>)\n", []string{readme + " md-link"}},
		{"コードブロックの中は見ない", "```\n[a](docs/none.md)\n[b](javascript:x)\n```\n", nil},
		{"チルダのコードブロック", "~~~\n[a](docs/none.md)\n~~~\n", nil},
		{"長い fence の中に短い fence", "````\n```\n[a](docs/none.md)\n```\n````\n", nil},
		{"コードブロックの後は見る", "```\nx\n```\n[a](docs/none.md)\n", []string{readme + " md-link"}},
		{"行内のコードは見ない", "`[a](docs/none.md)` と ``[b](javascript:x)``\n", nil},
		{"エスケープされた [ は見ない", "\\[a\\](docs/none.md)\n", nil},
		{"1 行に複数", "[a](docs/none1.md) と [b](docs/none2.md)\n", []string{readme + " md-link", readme + " md-link"}},
		{"%エンコード", "[a](docs/adr/0001-a%2Emd)\n", nil},
		{"%エンコードのリンク切れ", "[a](docs/adr/0001%2Da.md%00)\n", []string{readme + " md-link"}},
		{"不正な %", "[a](docs/%zz.md)\n", []string{readme + " md-link"}},
		{"生成物へのリンクは、まだ無くても実在する", "[a](docs/reference/core.md) [b](docs/reference/README.md) [c](docs/reference)\n", nil},
		{"生成物ではない docs/reference の中は、リンク切れ", "[a](docs/reference/none.md)\n", []string{readme + " md-link"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				"core/doc.go":        goodDoc("core"),
				"docs/adr/0001-a.md": "# 0001. a\n- 状態: 採用\n",
				readme:               tc.md,
			}
			expectRules(t, lintFindings(t, files), tc.want...)
		})
	}
}

// TestLintGeneratedLinks は、生成物 (索引) の相対リンクが、生成物どうしとして実在すると見なされ、リンク切れにならないことを
// 確認する。(go/doc/comment は、scheme:// の無いリンク定義を認めないので、doc comment の相対リンクが生成物に出る入口は無い。)
func TestLintGeneratedLinks(t *testing.T) {
	res, err := analyzeFiles(t, map[string]string{"core/doc.go": goodDoc("core")})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		if f.Rule == ruleMDLink {
			t.Errorf("生成物の相対リンクが切れている: %s", f)
		}
	}
}

// TestFindingOutput は、finding の出力の形 (path:行: [規則] 説明) と、並び順を確認する。
func TestFindingOutput(t *testing.T) {
	fs := []finding{
		{Path: "b.go", Line: 2, Rule: ruleExportedDoc, Msg: "x"},
		{Path: "a.md", Rule: ruleMDSize, Msg: "y"},
		{Path: "b.go", Line: 1, Rule: ruleExportedDoc, Msg: "z"},
		{Path: "a.md", Rule: ruleMDLink, Msg: "w"},
	}
	sortFindings(fs)
	var got []string
	for _, f := range fs {
		got = append(got, f.String())
	}
	want := []string{
		"a.md: [md-link] w",
		"a.md: [md-size] y",
		"b.go:1: [exported-doc] z",
		"b.go:2: [exported-doc] x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestLintLinkMessages は、リンクの問題の種類 (host を指す //・絶対 path・repo の外・scheme) が、区別して報告されることを確認する。
func TestLintLinkMessages(t *testing.T) {
	exists := func(string) (bool, error) { return false, nil }
	cases := map[string]string{
		"//evil.example/x": "host を指す",
		"/etc/passwd":      "絶対 path",
		"../outside.md":    "repo の外",
		"docs/../../x.md":  "repo の外",
		"javascript:x":     "scheme",
		"docs/none.md":     "リンク先 docs/none.md が無い",
	}
	for target, want := range cases {
		fs, err := lintLinks("README.md", "[a]("+target+")\n", exists)
		if err != nil || len(fs) != 1 || !strings.Contains(fs[0].Msg, want) {
			t.Errorf("%s: %v, %v (want %q を含む 1 件)", target, fs, err, want)
		}
	}
	// exists の error は、そのまま返す (予算超過を、リンク切れにしない)。
	if _, err := lintLinks("README.md", "[a](x.md)\n", func(string) (bool, error) { return false, errBudget }); !errors.Is(err, errBudget) {
		t.Errorf("exists の error を返すべき: %v", err)
	}
}

// TestLintMarkdownChecksGeneratedLinks は、生成物 (res.Expected) の中の相対リンクも、検査されることを確認する
// (レンダラに、リンクを壊す不具合があった場合の防御。いまの生成物には、doc comment 由来の相対リンクは出ない)。
func TestLintMarkdownChecksGeneratedLinks(t *testing.T) {
	tr, _ := newTestTree(t, scaffold(map[string]string{"core/doc.go": goodDoc("core")}))
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	res.Expected["docs/reference/broken.md"] = "[a](none.md) [b](core.md)\n"
	fs, err := tr.lintMarkdown(res)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		if f.Rule == ruleMDLink {
			got = append(got, f.Path+": "+f.Msg)
		}
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "docs/reference/broken.md: リンク先 none.md が無い") {
		t.Errorf("生成物のリンク切れ = %q (none.md の 1 件だけ。core.md は生成物として実在する)", got)
	}
}

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// generatedRepo は、package core と ADR 1 つを持つ repo を作り、docgen で生成して、root を返す。
func generatedRepo(t *testing.T, extra map[string]string) string {
	t.Helper()
	files := map[string]string{
		"core/doc.go":        goodDoc("core"),
		"docs/adr/0001-a.md": "# 0001. 最初\n- 状態: 採用\n",
		"README.md":          "# 短い README\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	root := fixtureRepo(t, scaffold(files))
	if code, _, errs := runIn(t, filepath.Join(root, "tools", "docgen")); code != 0 {
		t.Fatalf("生成に失敗: code = %d, stderr = %q", code, errs)
	}
	return root
}

// checkIn は、root で docgen -check を実行する。
func checkIn(t *testing.T, root string) (code int, stdout, stderr string) {
	t.Helper()
	return runIn(t, filepath.Join(root, "tools", "docgen"), "-check")
}

func writeFileAt(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileAt(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckClean(t *testing.T) {
	root := generatedRepo(t, nil)
	code, out, errs := checkIn(t, root)
	if code != 0 || errs != "" || !strings.Contains(out, "問題なし") {
		t.Errorf("code = %d, stdout = %q, stderr = %q", code, out, errs)
	}
	// 何度でも同じ。
	if code, _, _ := checkIn(t, root); code != 0 {
		t.Errorf("2 回目: code = %d", code)
	}
}

// TestCheckDetects は、docs-check が赤になる場面 (exit 1 で、ファイル名と規則が出る) を確認する。
// 受け入れ条件の 5 つ (①doc comment を変えて再生成しない ②生成物を手で編集 ③README が 2,001 字
// ④索引の生成区間を手で編集 ⑤doc comment に ESC) を含む。
func TestCheckDetects(t *testing.T) {
	core := "docs/reference/core.md"
	cases := []struct {
		name   string
		mutate func(t *testing.T, root string)
		want   []string // stderr に含まれるべき文字列
	}{
		{"①doc comment を変えて、再生成しない", func(t *testing.T, root string) {
			writeFileAt(t, root, "core/doc.go", strings.Replace(goodDoc("core"), "方針の段落。", "変えた方針の段落。", 1))
		}, []string{core, "[gen-stale]"}},
		{"②生成物を手で編集する", func(t *testing.T, root string) {
			writeFileAt(t, root, core, readFileAt(t, root, core)+"手で足した行\n")
		}, []string{core, "[gen-stale]"}},
		{"②の変形: 1 文字だけ変える", func(t *testing.T, root string) {
			writeFileAt(t, root, core, strings.Replace(readFileAt(t, root, core), "テスト", "テスタ", 1))
		}, []string{core, "[gen-stale]"}},
		{"②の変形: 空にする", func(t *testing.T, root string) {
			writeFileAt(t, root, core, "")
		}, []string{core, "[gen-stale]"}},
		{"③README が 2,001 字", func(t *testing.T, root string) {
			writeFileAt(t, root, "README.md", strings.Repeat("あ", 2000)+"\n")
		}, []string{"README.md", "[md-size]", "2001 字"}},
		{"④索引の生成区間を手で編集する", func(t *testing.T, root string) {
			p := "docs/adr/README.md"
			writeFileAt(t, root, p, strings.Replace(readFileAt(t, root, p), "| 最初 |", "| 手で変えた題 |", 1))
		}, []string{"docs/adr/README.md", "[gen-stale]", "索引の生成区間が古い"}},
		{"④の変形: 行を足す", func(t *testing.T, root string) {
			p := "docs/adr/README.md"
			writeFileAt(t, root, p, strings.Replace(readFileAt(t, root, p), adrEnd, "| 偽 | 手書きの行 | 採用 |\n"+adrEnd, 1))
		}, []string{"docs/adr/README.md", "[gen-stale]"}},
		{"⑤doc comment に ESC を入れる", func(t *testing.T, root string) {
			writeFileAt(t, root, "core/doc.go", strings.Replace(goodDoc("core"), "方針の段落。", "方針\x1b[31m赤", 1))
		}, []string{"core/doc.go:"}},
		{"生成物を消す", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "docs", "reference", "core.md")); err != nil {
				t.Fatal(err)
			}
		}, []string{core, "[gen-missing]"}},
		{"生成物の置き場ごと消す", func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, "docs", "reference")); err != nil {
				t.Fatal(err)
			}
		}, []string{core, "docs/reference/README.md", "[gen-missing]"}},
		{"余剰のファイル", func(t *testing.T, root string) {
			writeFileAt(t, root, "docs/reference/notes.md", "手で書いた文書\n")
		}, []string{"docs/reference/notes.md", "[gen-extra]", "生成物ではないファイル"}},
		{"余剰の隠しファイル", func(t *testing.T, root string) {
			writeFileAt(t, root, "docs/reference/.hidden", "x\n")
		}, []string{"docs/reference/.hidden", "[gen-extra]"}},
		{"余剰の .md 以外", func(t *testing.T, root string) {
			writeFileAt(t, root, "docs/reference/img.png", "x\n")
		}, []string{"docs/reference/img.png", "[gen-extra]"}},
		{"余剰のディレクトリ (空)", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "docs", "reference", "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, []string{"docs/reference/empty", "[gen-extra]", "生成物ではないディレクトリ"}},
		{"余剰のディレクトリと、中のファイル", func(t *testing.T, root string) {
			writeFileAt(t, root, "docs/reference/sub/deep/x.md", "x\n")
		}, []string{"docs/reference/sub", "docs/reference/sub/deep/x.md", "[gen-extra]"}},
		{"生成物が、ディレクトリに置き換わる", func(t *testing.T, root string) {
			p := filepath.Join(root, "docs", "reference", "core.md")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}, []string{core, "[gen-missing]", "[gen-extra]"}},
		{"CRLF に変換される", func(t *testing.T, root string) {
			writeFileAt(t, root, core, strings.ReplaceAll(readFileAt(t, root, core), "\n", "\r\n"))
		}, []string{core, "[gen-stale]"}},
		{"新しい package を足して、再生成しない", func(t *testing.T, root string) {
			writeFileAt(t, root, "core/sub/go.mod", "module example.com/m/coresub\n")
			writeFileAt(t, root, "core/sub/doc.go", goodDoc("sub"))
		}, []string{"docs/reference/core/sub.md", "[gen-missing]", "docs/reference/README.md", "[gen-stale]"}},
		{"package を消して、再生成しない", func(t *testing.T, root string) {
			writeFileAt(t, root, "core/sub/go.mod", "module example.com/m/coresub\n")
			writeFileAt(t, root, "core/sub/doc.go", goodDoc("sub"))
			if code, _, errs := runIn(t, filepath.Join(root, "tools", "docgen")); code != 0 {
				t.Fatalf("再生成: %d %s", code, errs)
			}
			if err := os.RemoveAll(filepath.Join(root, "core", "sub")); err != nil {
				t.Fatal(err)
			}
		}, []string{"docs/reference/core/sub.md", "[gen-extra]", "docs/reference/README.md", "[gen-stale]"}},
		{"ADR を足して、再生成しない", func(t *testing.T, root string) {
			writeFileAt(t, root, "docs/adr/0002-b.md", "# 0002. 二つ目\n- 状態: 提案\n")
		}, []string{"docs/adr/README.md", "[gen-stale]"}},
		{"生成物にだけ、制御文字を入れる", func(t *testing.T, root string) {
			writeFileAt(t, root, core, strings.Replace(readFileAt(t, root, core), "# `core`", "# `core`\x1b[2J", 1))
		}, []string{core, "[gen-stale]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := generatedRepo(t, nil)
			tc.mutate(t, root)
			before := readTree(t, root)
			code, out, errs := checkIn(t, root)
			if code != 1 {
				t.Errorf("code = %d, want 1 (stdout = %q, stderr = %q)", code, out, errs)
			}
			for _, w := range tc.want {
				if !strings.Contains(errs, w) {
					t.Errorf("stderr に %q が無い:\n%s", w, errs)
				}
			}
			for _, r := range errs {
				if forbiddenRune(r) {
					t.Errorf("stderr に U+%04X が残った", r)
				}
			}
			// -check は、何も書かない。
			if after := readTree(t, root); !reflect.DeepEqual(before, after) {
				t.Error("-check が、ファイルを書き換えた")
			}
		})
	}
}

// TestCheckAllowsHandWrittenOutsideRegion は、ADR の README の、生成区間の外は手書きなので、変えても通ることを確認する
// (限界ではなく仕様。区間の外の文書の上限は md-size が見る)。
func TestCheckAllowsHandWrittenOutsideRegion(t *testing.T) {
	root := generatedRepo(t, nil)
	p := "docs/adr/README.md"
	writeFileAt(t, root, p, "手書きの前置き。\n"+readFileAt(t, root, p)+"\n手書きの後書き。\n")
	if code, out, errs := checkIn(t, root); code != 0 {
		t.Errorf("code = %d, stdout = %q, stderr = %q", code, out, errs)
	}
}

// TestCheckReportsAll は、問題が複数あれば、最初の 1 件で止まらず、すべて報告することを確認する。
func TestCheckReportsAll(t *testing.T) {
	root := generatedRepo(t, nil)
	writeFileAt(t, root, "docs/reference/core.md", "壊れた\n")
	writeFileAt(t, root, "docs/reference/extra.md", "余剰\n")
	writeFileAt(t, root, "README.md", strings.Repeat("あ\n", 40))
	writeFileAt(t, root, "docs/other.md", "x\n")
	code, _, errs := checkIn(t, root)
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	for _, w := range []string{"[gen-stale]", "[gen-extra]", "[md-size]", "[md-place]", "docs/other.md", "docs/reference/extra.md", "件の問題"} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr に %q が無い:\n%s", w, errs)
		}
	}
	// 並びが決まっている (path の順)。
	lines := strings.Split(strings.TrimSpace(errs), "\n")
	body := lines[:len(lines)-1]
	if !slices.IsSorted(body) {
		t.Errorf("出力が path の順ではない:\n%s", errs)
	}
}

// TestCheckErrorsAreNotFindings は、生成物の置き場の symlink など、読めないものが、問題の一覧ではなく error
// (何も比べられない) になり、exit 1 で、ファイル名が出ることを確認する。
func TestCheckErrorsAreNotFindings(t *testing.T) {
	root := generatedRepo(t, nil)
	if err := os.Remove(filepath.Join(root, "docs", "reference", "core.md")); err != nil {
		t.Fatal(err)
	}
	symlink(t, "../../README.md", filepath.Join(root, "docs", "reference", "core.md"))
	code, out, errs := checkIn(t, root)
	if code != 1 || out != "" || !strings.Contains(errs, "docs/reference/core.md") {
		t.Errorf("code = %d, stdout = %q, stderr = %q", code, out, errs)
	}
}

// findingsAfter は、files の repo を生成し、mutate した後の、lint と照合の結果を、規則の集合にして返す。
func findingsAfter(t *testing.T, files map[string]string, mutate func(root string)) map[rule]bool {
	t.Helper()
	all := map[string]string{"core/doc.go": goodDoc("core"), "README.md": "# 短い\n"}
	for k, v := range files {
		all[k] = v
	}
	root := fixtureRepo(t, scaffold(all))
	if mutate != nil {
		if code, _, errs := runIn(t, filepath.Join(root, "tools", "docgen")); code != 0 {
			t.Fatalf("生成に失敗: %d %s", code, errs)
		}
		mutate(root)
	}
	tr := newTree(openTestRoot(t, root), defaultLimits)
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	cmp, err := tr.compare(res)
	if err != nil {
		t.Fatal(err)
	}
	got := map[rule]bool{}
	for _, f := range append(res.Findings, cmp...) {
		got[f.Rule] = true
	}
	return got
}

// TestEveryRuleFires は、allRules のすべての規則が、最小の場面で、実際に報告されることを確認する
// (規則を足して、報告する場面のテストを足し忘れると、赤になる。doc.go の規則の一覧とも、doc_test.go で一致を見る)。
func TestEveryRuleFires(t *testing.T) {
	doc := "core/doc.go"
	cases := map[rule]struct {
		files  map[string]string
		mutate func(root string)
	}{
		rulePkgDocMissing:  {files: map[string]string{doc: "package core\n"}},
		rulePkgDocMultiple: {files: map[string]string{"core/b.go": docComment("Package core は、b。", "", "契約。") + "package core\n"}},
		rulePkgDocStart:    {files: map[string]string{doc: docComment("core は、テスト。", "", "契約。") + "package core\n"}},
		rulePkgDocContract: {files: map[string]string{doc: docComment("Package core は、テスト。") + "package core\n"}},
		rulePkgDocHeading:  {files: map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 未定義", "", "本文。") + "package core\n"}},
		rulePkgDocUsage:    {files: map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 使い方", "", "\t1", "\t2", "\t3", "\t4", "\t5", "\t6") + "package core\n"}},
		rulePkgDocRuleItem: {files: map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - 説明だけ。") + "package core\n"}},
		rulePkgDocRuleDup:  {files: map[string]string{doc: docComment("Package core は、テスト。", "", "契約。", "", "# 規則", "", "  - a: 1。", "  - a: 2。") + "package core\n"}},
		rulePkgDocLimits: {files: map[string]string{
			"tools/go.mod": "module example.com/m/tools\n", "tools/archtest/doc.go": docComment("Package archtest は、テスト。", "", "契約。") + "package archtest\n"}},
		rulePkgDocLength: {files: map[string]string{doc: docWithLines("core", 31)}},
		ruleExportedDoc:  {files: map[string]string{"core/x.go": "package core\n\nfunc F() {}\n"}},
		ruleMDPlace:      {files: map[string]string{"docs/other.md": "x\n"}},
		ruleMDADRName:    {files: map[string]string{"docs/adr/notes.md": "x\n"}},
		ruleMDSize:       {files: map[string]string{"README.md": strings.Repeat("あ\n", 31)}},
		ruleMDLink:       {files: map[string]string{"README.md": "[x](none.md)\n"}},
		ruleGenMissing: {mutate: func(root string) {
			os.Remove(filepath.Join(root, "docs", "reference", "core.md"))
		}},
		ruleGenStale: {mutate: func(root string) {
			os.WriteFile(filepath.Join(root, "docs", "reference", "core.md"), []byte("x\n"), 0o644)
		}},
		ruleGenExtra: {mutate: func(root string) {
			os.WriteFile(filepath.Join(root, "docs", "reference", "x.md"), []byte("x\n"), 0o644)
		}},
	}
	for _, r := range allRules {
		tc, ok := cases[r]
		if !ok {
			t.Errorf("規則 %s を報告する場面が、この表に無い", r)
			continue
		}
		if !findingsAfter(t, tc.files, tc.mutate)[r] {
			t.Errorf("規則 %s が、最小の場面で報告されなかった", r)
		}
	}
	if len(cases) != len(allRules) {
		t.Errorf("表の規則が %d 個、allRules が %d 個 (食い違い)", len(cases), len(allRules))
	}
	// 対照: 何も起こさない repo は、どの規則も報告しない。
	if got := findingsAfter(t, nil, func(string) {}); len(got) != 0 {
		t.Errorf("問題の無い repo で、規則が報告された: %v", got)
	}
}

func TestFirstDiffLine(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a\nb\n", "a\nb\n", 0},
		{"a\nb\n", "a\nc\n", 2},
		{"a\nb\n", "x\nb\n", 1},
		{"a\nb\nc\n", "a\nb\n", 3},
		{"a\nb\n", "a\nb\nc\n", 3},
		{"a\r\nb\n", "a\nb\n", 1},
		{"a", "a\n", 2},
	}
	for _, tc := range cases {
		if got := firstDiffLine(tc.a, tc.b); got != tc.want {
			t.Errorf("firstDiffLine(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestCheckReportsLine は、生成物が古いときに、最初に違う行が、path:行: の形で出ることを確認する。
func TestCheckReportsLine(t *testing.T) {
	root := generatedRepo(t, nil)
	p := "docs/reference/core.md"
	lines := strings.Split(readFileAt(t, root, p), "\n")
	lines[4] = "手で変えた行 (同じ行数のまま)"
	writeFileAt(t, root, p, strings.Join(lines, "\n"))
	_, _, errs := checkIn(t, root)
	if !strings.Contains(errs, "docs/reference/core.md:5: [gen-stale]") {
		t.Errorf("行番号が出ていない:\n%s", errs)
	}
}

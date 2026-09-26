package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeRepo は、scaffold の repo (package core と ADR 1 つ) を作り、tree を返す。
func writeRepo(t *testing.T, extra map[string]string) (*tree, string) {
	t.Helper()
	files := map[string]string{
		"core/doc.go":        "// Package core は、テスト。\npackage core\n",
		"docs/adr/0001-a.md": "# 0001. 最初\n- 状態: 採用\n",
		"README.md":          "# 手書きの README\n",
		"docs/adr/README.md": "# ADR\n\n前の文。\n\n" + adrBegin + "\n古い\n" + adrEnd + "\n\n後ろの文。\n",
		"core/notes.txt":     "無関係\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	return newTestTree(t, scaffold(files))
}

func TestWriteOutputsRemovesStale(t *testing.T) {
	tr, dir := writeRepo(t, map[string]string{
		"docs/reference/old.md":            "消える\n",
		"docs/reference/gone/x.md":         "消える\n",
		"docs/reference/gone/deep/y.txt":   "消える\n",
		"docs/reference/.hidden":           "消える\n",
		"docs/reference/core.md":           "古い内容\n",
		"docs/reference/README.md":         "古い索引\n",
		"docs/reference/keepdir/README.md": "消える\n",
	})
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.writeOutputs(res); err != nil {
		t.Fatal(err)
	}
	got := readTree(t, filepath.Join(dir, "docs", "reference"))
	if want := []string{"README.md", "core.md"}; !reflect.DeepEqual(sortedKeys(got), want) {
		t.Errorf("生成物の置き場 = %v, want %v", sortedKeys(got), want)
	}
	if got["core.md"] != res.Expected["docs/reference/core.md"] {
		t.Error("core.md が、期待する内容ではない")
	}
	// 空になったディレクトリも消える。
	for _, d := range []string{"gone", "gone/deep", "keepdir"} {
		if _, err := os.Lstat(filepath.Join(dir, "docs", "reference", d)); err == nil {
			t.Errorf("空のディレクトリ %s が残っている", d)
		}
	}
}

// TestWriteOutputsKeepsDirs は、生成物が要るディレクトリ (cmd/tool.md の cmd/) は消さないことを確認する。
func TestWriteOutputsKeepsDirs(t *testing.T) {
	tr, dir := writeRepo(t, map[string]string{
		"cmd/go.mod":                  "module example.com/m/cmd\n",
		"cmd/tool/main.go":            "// Command tool は、テスト。\npackage main\n",
		"docs/reference/cmd/stale.md": "消える\n",
		"docs/reference/other/x.md":   "消える\n",
	})
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.writeOutputs(res); err != nil {
		t.Fatal(err)
	}
	got := sortedKeys(readTree(t, filepath.Join(dir, "docs", "reference")))
	if want := []string{"README.md", "cmd/tool.md", "core.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("生成物の置き場 = %v, want %v", got, want)
	}
}

// TestWriteOutputsTouchesOnlyGenerated は、生成物の置き場と ADR の README の生成区間の外を、何も変えないことを確認する。
func TestWriteOutputsTouchesOnlyGenerated(t *testing.T) {
	tr, dir := writeRepo(t, nil)
	before := readTree(t, dir)
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.writeOutputs(res); err != nil {
		t.Fatal(err)
	}
	after := readTree(t, dir)
	for p, b := range before {
		if a, ok := after[p]; !ok {
			t.Errorf("%s が消えた", p)
		} else if a != b && p != adrReadme {
			t.Errorf("%s が変わった", p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok && !strings.HasPrefix(p, referenceDir+"/") {
			t.Errorf("%s が増えた (生成物の置き場の外)", p)
		}
	}
	readme := after[adrReadme]
	if !strings.HasPrefix(readme, "# ADR\n\n前の文。\n\n"+adrBegin+"\n| 番号 |") || !strings.HasSuffix(readme, adrEnd+"\n\n後ろの文。\n") {
		t.Errorf("ADR の README の、区間の外が変わった:\n%s", readme)
	}
	if strings.Contains(readme, "古い") {
		t.Error("区間の中が置き換わっていない")
	}
}

// TestWriteOutputsHandEditedIsOverwritten は、手で編集した生成物が、生成で元に戻ることを確認する。
func TestWriteOutputsHandEditedIsOverwritten(t *testing.T) {
	tr, dir := writeRepo(t, nil)
	res, err := tr.analyze()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.writeOutputs(res); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "docs", "reference", "core.md")
	if err := os.WriteFile(p, []byte("手で編集した\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr2 := newTree(openTestRoot(t, dir), defaultLimits)
	res2, err := tr2.analyze()
	if err != nil {
		t.Fatal(err)
	}
	written, _, err := tr2.writeOutputs(res2)
	if err != nil || written != 1 {
		t.Fatalf("written = %d, err = %v", written, err)
	}
	if b, _ := os.ReadFile(p); string(b) != res.Expected["docs/reference/core.md"] {
		t.Errorf("元に戻っていない: %q", b)
	}
}

// TestWriteOutputRejectsForbidden は、writeOutput (ファイルへ書く唯一の入口) が、どの部品を通ったかによらず、
// 全体に禁止する文字を含む内容を、書かずに error にすることを確認する (最後の関門)。
func TestWriteOutputRejectsForbidden(t *testing.T) {
	tr, dir := newTestTree(t, nil)
	for name, content := range map[string]string{
		"ESC": "a\x1b[31mb\n", "BEL": "a\x07\n", "BS": "a\x08\n", "RLO": "a\u202eb\n", "CR": "a\r\n", "invalid": "a\xff\n", "改行なし": "a",
	} {
		if err := writeOutput(tr, "docs/reference/x.md", content); err == nil {
			t.Errorf("%s: error にすべき", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "docs")); err == nil {
		t.Error("error なのに、ファイルかディレクトリができた")
	}
	if err := writeOutput(tr, "docs/reference/x.md", "正常\n"); err != nil {
		t.Errorf("正常な内容: %v", err)
	}
}

// TestGenerateRefusesSymlinkedOutput は、生成物の置き場に symlink があると、何も書かずに error になることを確認する。
func TestGenerateRefusesSymlinkedOutput(t *testing.T) {
	root := fixtureRepo(t, scaffold(map[string]string{
		"core/doc.go": "// Package core は、テスト。\npackage core\n",
		"README.md":   "# 守るべき README\n",
	}))
	symlink(t, "../../README.md", filepath.Join(root, "docs", "reference", "core.md"))
	code, out, errs := runIn(t, filepath.Join(root, "tools", "docgen"))
	if code != 1 || out != "" || errs == "" {
		t.Errorf("code = %d, stdout = %q, stderr = %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "README.md")); string(b) != "# 守るべき README\n" {
		t.Errorf("README.md が変わった: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(root, "docs", "reference", "README.md")); err == nil {
		t.Error("error なのに、他の生成物を書いた")
	}
}

// TestGenerateWithSymlinkedDocs は、docs が symlink なら、辿らずに error になることを確認する。
func TestGenerateWithSymlinkedDocs(t *testing.T) {
	root := fixtureRepo(t, map[string]string{"core/doc.go": "// Package core は、テスト。\npackage core\n", "core/go.mod": "module example.com/m/core\n"})
	if err := os.MkdirAll(filepath.Join(root, "elsewhere", "adr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "elsewhere", "adr", "README.md"), []byte(adrBegin+"\n"+adrEnd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, "elsewhere", filepath.Join(root, "docs"))
	if code, _, errs := runIn(t, filepath.Join(root, "tools", "docgen")); code != 1 || errs == "" {
		t.Errorf("code = %d, stderr = %q", code, errs)
	}
	if got := readTree(t, filepath.Join(root, "elsewhere")); len(got) != 1 {
		t.Errorf("symlink の先に、ファイルができた: %v", sortedKeys(got))
	}
}

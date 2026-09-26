package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeRepo は、scaffold の repo (package core と ADR 1 つ) を作り、tree を返す。
func writeRepo(t *testing.T, extra map[string]string) (*tree, string) {
	t.Helper()
	return writeRepoLimits(t, extra, defaultLimits)
}

// writeRepoLimits は、writeRepo と同じ repo を、予算 lim の tree で開く。
func writeRepoLimits(t *testing.T, extra map[string]string, lim limits) (*tree, string) {
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
	return newTestTreeLimits(t, scaffold(files), lim)
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

// TestWriteOutputsWritesNothingWhenPlanFails は、書く前に分かる失敗 (予算・既存の項目との衝突) があれば、何も書かず、
// 何も消さず、ディレクトリも作らないことを確認する (一部だけが書かれた出力が残らない)。
// どの場面も、解析 (読む) は通り、書く段階で失敗する。書く予定の先頭は ADR の README (区間が古い) なので、
// 確かめる前に書き始める実装は、それを書き換えてしまう。
func TestWriteOutputsWritesNothingWhenPlanFails(t *testing.T) {
	stale := map[string]string{"docs/reference/old.md": "消える\n"} // 余剰の削除も、書く予定に入る
	// run は、解析が通り、writeOutputs が error になること、何も変わっていないことを確かめて、error を返す。
	run := func(t *testing.T, tr *tree, dir string) error {
		t.Helper()
		before := readTree(t, dir)
		_, refBefore := os.Lstat(filepath.Join(dir, "docs", "reference"))
		res, err := tr.analyze()
		if err != nil {
			t.Fatalf("解析は通るはず (書く段階で失敗する場面): %v", err)
		}
		_, _, err = tr.writeOutputs(res)
		if err == nil {
			t.Fatal("error にすべき")
		}
		if after := readTree(t, dir); !reflect.DeepEqual(before, after) {
			t.Errorf("error なのに、ファイルを書き換えた・消した (before %v, after %v)", sortedKeys(before), sortedKeys(after))
		}
		if _, refAfter := os.Lstat(filepath.Join(dir, "docs", "reference")); (refBefore == nil) != (refAfter == nil) {
			t.Error("error なのに、docs/reference ができた・消えた")
		}
		return err
	}

	// 予算は、全体を成功させて勘定を測り、その 1 つ手前を上限にする。解析は足りて、最後の 1 つを書くところで足りなくなる。
	full, _ := writeRepo(t, stale)
	res, err := full.analyze()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := full.writeOutputs(res); err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]func(l *limits){
		"項目の数":   func(l *limits) { l.MaxEntries = full.entries - 1 },
		"ファイルの数": func(l *limits) { l.MaxFiles = full.files - 1 },
		"バイトの合計": func(l *limits) { l.MaxTotalBytes = full.bytes - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			lim := defaultLimits
			set(&lim)
			tr, dir := writeRepoLimits(t, stale, lim)
			if err := run(t, tr, dir); !errors.Is(err, errBudget) {
				t.Errorf("errBudget にすべき: %v", err)
			}
		})
	}

	t.Run("生成物が、1 ファイルの上限を超える", func(t *testing.T) {
		// 入力は上限に収まるが、エスケープで、出力が大きくなる (* は \* になる)。
		lim := defaultLimits
		lim.MaxFileBytes = 1500
		tr, dir := writeRepoLimits(t, map[string]string{
			"core/doc.go": docComment("Package core は、テスト。", "", strings.Repeat("*", 900)) + "package core\n",
		}, lim)
		if err := run(t, tr, dir); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %v", err)
		}
	})

	t.Run("生成物と同じ名前のディレクトリがある", func(t *testing.T) {
		tr, dir := writeRepo(t, map[string]string{"docs/reference/core.md/keep.txt": "x\n"})
		if err := run(t, tr, dir); !strings.Contains(err.Error(), "同じ名前のディレクトリ") {
			t.Errorf("error 文: %v", err)
		}
	})

	t.Run("生成物の親のディレクトリと同じ名前の、通常のファイルがある", func(t *testing.T) {
		tr, dir := writeRepo(t, map[string]string{
			"core/sub/doc.go":     "// Package sub は、テスト。\npackage sub\n", // docs/reference/core/sub.md を作る
			"docs/reference/core": "邪魔\n",
		})
		if err := run(t, tr, dir); !strings.Contains(err.Error(), "通常のファイル") {
			t.Errorf("error 文: %v", err)
		}
	})

	t.Run("既存の生成物が、1 ファイルの上限を超える (比べるための読み取りが失敗する)", func(t *testing.T) {
		tr, dir := writeRepo(t, map[string]string{"docs/reference/core.md": strings.Repeat("x", int(defaultLimits.MaxFileBytes)+1)})
		if err := run(t, tr, dir); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %v", err)
		}
	})
}

// TestCheckPlan は、checkPlan の予算の試算が、実際に書く・消すときの勘定と過不足なく一致し (ちょうどは通し、1 つ足りなければ
// errBudget)、tree そのものの勘定を変えないことと、書く内容の検査をすることを確認する。試算が多く数えると、実際には足りる
// repo を error にし、少なく数えると、書き始めた後に予算で止まる (一部だけが書かれた出力が残る)。
func TestCheckPlan(t *testing.T) {
	expected := map[string]string{"docs/reference/a.md": "aaa\n", "docs/reference/b.md": "bb\n"}
	writes := []string{"docs/reference/a.md", "docs/reference/b.md"}
	removes := []refEntry{{Path: "docs/reference/x.md"}, {Path: "docs/reference/y", Dir: true}}

	// 実際に書く・消すときの勘定を測る (書く 2 つ・消す 2 つ)。
	actual, dir := newTestTree(t, map[string]string{"docs/reference/x.md": "x\n"})
	if err := os.MkdirAll(filepath.Join(dir, "docs", "reference", "y"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range writes {
		if err := actual.writeFile(p, []byte(expected[p])); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range removes {
		if err := actual.remove(e.Path); err != nil {
			t.Fatal(err)
		}
	}

	for name, set := range map[string]func(l *limits, delta int){
		"項目の数":   func(l *limits, d int) { l.MaxEntries = actual.entries + d },
		"ファイルの数": func(l *limits, d int) { l.MaxFiles = actual.files + d },
		"バイトの合計": func(l *limits, d int) { l.MaxTotalBytes = actual.bytes + int64(d) },
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				delta int
				ok    bool
			}{{0, true}, {-1, false}} {
				lim := defaultLimits
				set(&lim, tc.delta)
				tr, _ := newTestTreeLimits(t, nil, lim)
				err := tr.checkPlan(expected, writes, removes, nil, nil)
				if tc.ok && err != nil || !tc.ok && !errors.Is(err, errBudget) {
					t.Errorf("実際の勘定 %+d の予算: error = %v (ok = %v)", tc.delta, err, tc.ok)
				}
				if tr.entries != 0 || tr.files != 0 || tr.bytes != 0 {
					t.Errorf("checkPlan が、tree の勘定を変えた: 項目 %d・ファイル %d・バイト %d", tr.entries, tr.files, tr.bytes)
				}
			}
		})
	}

	t.Run("書く内容が、文書として不正", func(t *testing.T) {
		tr, _ := newTestTree(t, nil)
		bad := map[string]string{"docs/reference/a.md": "改行で終わらない"}
		if err := tr.checkPlan(bad, []string{"docs/reference/a.md"}, nil, nil, nil); err == nil {
			t.Error("error にすべき")
		}
	})
}

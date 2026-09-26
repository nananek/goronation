package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInventory(t *testing.T) {
	tr, _ := newTestTree(t, map[string]string{
		"go.work":                 "go 1.24.0\n",
		"README.md":               "# x\n",
		"core/go.mod":             "module m/core\n",
		"core/doc.go":             "package core\n",
		"core/x_test.go":          "package core\n",
		"core/notes.txt":          "無視される\n",
		"docs/adr/0001-a.md":      "# 0001. a\n",
		"docs/reference/core.md":  "生成物は、別に数える\n",
		"docs/OTHER.MD":           "拡張子の大文字小文字は区別しない\n",
		"docs/x.markdown":         "x\n",
		"sandbox/bwrap/doc.go":    "package bwrap\n",
		"testdata/x.go":           "package x\n",
		"testdata/README.md":      "辿らない\n",
		"vendor/v/v.go":           "package v\n",
		".git/config":             "辿らない\n",
		".github/workflows/w.md":  "辿らない\n",
		"_skip/y.go":              "package y\n",
		".hidden/z.go":            "package z\n",
		"core/sub/testdata/t.go":  "package t\n",
		"tools/docgen/main.go":    "package main\n",
		"tools/docgen/go.mod":     "module m/tools/docgen\n",
		"tools/docgen/notes.MDX":  "対象外の拡張子\n",
		"tools/docgen/Doc.GO":     "拡張子 .GO は go tool も build に使わない\n",
		"tools/docgen/.hidden.go": "隠しファイルの名前でも、.go は読む\n",
		"tools/docgen/_under.go":  "_ で始まる .go も数える (数えすぎる側に倒す)\n",
	})
	inv, err := tr.inventory()
	if err != nil {
		t.Fatal(err)
	}
	want := &inventory{
		Go:       []string{"core/doc.go", "core/x_test.go", "sandbox/bwrap/doc.go", "tools/docgen/.hidden.go", "tools/docgen/_under.go", "tools/docgen/main.go"},
		Mod:      []string{"core/go.mod", "tools/docgen/go.mod"},
		Markdown: []string{"README.md", "docs/OTHER.MD", "docs/adr/0001-a.md", "docs/x.markdown"},
	}
	if !reflect.DeepEqual(inv, want) {
		t.Errorf("inventory =\n  %+v\nwant\n  %+v", inv, want)
	}
}

// TestInventoryDeterministic は、列挙の順序が、ファイルシステムの並びによらず、名前の順に固定されることを確認する。
func TestInventoryDeterministic(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"z", "a", "m", "B", "b", "10", "9"} {
		files[n+"/doc.go"] = "package x\n"
	}
	var first *inventory
	for range 3 {
		tr, _ := newTestTree(t, files)
		inv, err := tr.inventory()
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = inv
			continue
		}
		if !reflect.DeepEqual(first, inv) {
			t.Fatalf("実行ごとに違う: %v vs %v", first, inv)
		}
	}
	want := []string{"10/doc.go", "9/doc.go", "B/doc.go", "a/doc.go", "b/doc.go", "m/doc.go", "z/doc.go"}
	if !reflect.DeepEqual(first.Go, want) {
		t.Errorf("Go = %v, want %v (名前のバイト順)", first.Go, want)
	}
}

func TestInventoryPathChars(t *testing.T) {
	for _, name := range []string{"core/日本語.go", "core/a b.go", "core/a\x1bb.go", "core/a\nb.go", "core/a(b).go", "d ir/x.go", "core/a`b.md", "core/é.mod.go"} {
		t.Run(name, func(t *testing.T) {
			tr, _ := newTestTree(t, map[string]string{"core/doc.go": "package core\n", name: "package x\n"})
			if inv, err := tr.inventory(); err == nil {
				t.Errorf("error を返すべき (許可しない文字を含む path): %+v", inv)
			}
		})
	}
	// 読まない名前 (無視するもの) は、何の文字でもよい。
	tr, _ := newTestTree(t, map[string]string{"core/doc.go": "package core\n", "core/日本語 メモ.txt": "x\n"})
	if _, err := tr.inventory(); err != nil {
		t.Errorf("読まない名前は無視するべき: %v", err)
	}
}

func TestInventoryDepthLimit(t *testing.T) {
	lim := defaultLimits
	lim.MaxDepth = 3
	deep := map[string]string{"a/b/c/x.go": "package c\n"}
	tr, _ := newTestTreeLimits(t, deep, lim)
	if _, err := tr.inventory(); err != nil {
		t.Errorf("深さ 3 (上限ちょうど) は通すべき: %v", err)
	}
	tooDeep := map[string]string{"a/b/c/d/x.go": "package d\n"}
	tr, _ = newTestTreeLimits(t, tooDeep, lim)
	if _, err := tr.inventory(); !errors.Is(err, errBudget) {
		t.Errorf("深さ 4 は errBudget にすべき: %v", err)
	}
}

// TestBudgetIsShared は、予算が、操作ごとではなく、tree (実行全体) で共有されることを確認する。
// 読む・書く・ディレクトリを辿る操作が、同じ勘定に足される。
func TestBudgetIsShared(t *testing.T) {
	files := map[string]string{"a.go": "1", "b.go": "2", "c.go": "3", "d.go": "4"}

	t.Run("ファイルの数: 読むと書くと辿るが、同じ勘定", func(t *testing.T) {
		lim := defaultLimits
		lim.MaxFiles = 3
		tr, _ := newTestTreeLimits(t, files, lim)
		if _, err := tr.readFile("a.go"); err != nil {
			t.Fatal(err)
		}
		if err := tr.writeFile("out/x.md", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.inventory(); err != nil { // ファイルを読まない (数えない)
			t.Fatal(err)
		}
		if _, err := tr.readFile("b.go"); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.readFile("c.go"); !errors.Is(err, errBudget) {
			t.Errorf("4 つ目は errBudget にすべき: %v", err)
		}
	})

	t.Run("バイトの合計", func(t *testing.T) {
		lim := defaultLimits
		lim.MaxTotalBytes = 15
		tr, _ := newTestTreeLimits(t, map[string]string{"a.go": strings.Repeat("a", 10), "b.go": strings.Repeat("b", 10)}, lim)
		if _, err := tr.readFile("a.go"); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.readFile("b.go"); !errors.Is(err, errBudget) {
			t.Errorf("合計 20 バイトは errBudget にすべき: %v", err)
		}
	})

	t.Run("書くバイトも、読むバイトと同じ勘定", func(t *testing.T) {
		lim := defaultLimits
		lim.MaxTotalBytes = 15
		tr, _ := newTestTreeLimits(t, map[string]string{"a.go": strings.Repeat("a", 10)}, lim)
		if _, err := tr.readFile("a.go"); err != nil {
			t.Fatal(err)
		}
		if err := tr.writeFile("out.md", []byte(strings.Repeat("x", 10))); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(tr.root.Name(), "out.md")); err == nil {
			t.Error("予算を超えたら、書かない")
		}
	})

	t.Run("1 ファイルの大きさ (ちょうどは通し、1 バイト超は error)", func(t *testing.T) {
		lim := defaultLimits
		lim.MaxFileBytes = 10
		tr, _ := newTestTreeLimits(t, map[string]string{"ok.go": strings.Repeat("a", 10), "big.go": strings.Repeat("a", 11)}, lim)
		if _, err := tr.readFile("ok.go"); err != nil {
			t.Errorf("ちょうどは通すべき: %v", err)
		}
		if _, err := tr.readFile("big.go"); !errors.Is(err, errBudget) {
			t.Errorf("1 バイト超は errBudget にすべき: %v", err)
		}
		if tr.bytes != 10 {
			t.Errorf("bytes = %d: 上限を超えるファイルは、読む前に (宣言の大きさで) 断り、バイトを勘定に足さない", tr.bytes)
		}
	})

	// 確認 (fstat) の後に、ファイルが伸びても、上限を超えては読まない (メモリも、勘定も)。
	grow := func(t *testing.T, dir string) func(stage, name string) {
		return func(stage, name string) {
			if stage != stageBeforeRead {
				return
			}
			f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(strings.Repeat("x", 30)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("読む間に、1 ファイルの上限を超えて伸びる", func(t *testing.T) {
		lim := defaultLimits
		lim.MaxFileBytes = 20
		tr, dir := newTestTreeLimits(t, map[string]string{"a.go": strings.Repeat("a", 10)}, lim)
		tr.hook = grow(t, dir)
		if b, err := tr.readFile("a.go"); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %d バイト, %v", len(b), err)
		}
	})
	t.Run("読む間に伸びた分も、バイトの合計に足す", func(t *testing.T) {
		lim := defaultLimits
		lim.MaxTotalBytes = 15
		tr, dir := newTestTreeLimits(t, map[string]string{"a.go": strings.Repeat("a", 10)}, lim)
		tr.hook = func(stage, name string) {
			if stage == stageBeforeRead { // 10 バイト → 20 バイト (1 ファイルの上限には収まるが、合計 15 を超える)
				f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				f.WriteString(strings.Repeat("x", 10))
			}
		}
		if b, err := tr.readFile("a.go"); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %d バイト, %v", len(b), err)
		}
	})

	t.Run("項目の数: listDir をまたいで共有し、上限を超えた時点で止まる", func(t *testing.T) {
		many := map[string]string{}
		for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
			many["d/"+n] = "x"
		}
		lim := defaultLimits
		lim.MaxEntries = 12
		tr, _ := newTestTreeLimits(t, many, lim)
		if _, err := tr.listDir("d"); err != nil { // 1 (開く) + 10 (名前) = 11
			t.Fatal(err)
		}
		if _, err := tr.listDir("d"); !errors.Is(err, errBudget) { // 2 回目で、12 を超える
			t.Errorf("2 回目は errBudget にすべき: %v", err)
		}
		if tr.entries != 13 {
			t.Errorf("entries = %d, want 13 (超えた時点で止まる)", tr.entries)
		}
	})

	t.Run("項目が非常に多いディレクトリでも、上限を超えた時点で止まる", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 700 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		lim := defaultLimits
		lim.MaxEntries = 100
		tr := newTree(openTestRoot(t, dir), lim)
		if _, err := tr.listDir("."); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %v", err)
		}
		if tr.entries > 100+256 {
			t.Errorf("entries = %d: 256 個の塊を超えて、先まで数えた", tr.entries)
		}
	})

	t.Run("時間は、ファイルを触らない処理の合間にも確かめる", func(t *testing.T) {
		files := scaffold(map[string]string{"core/doc.go": goodDoc("core")})
		// 1 回目: 何回、時計を見るか (inventory と readSources の分) を数える。
		count := func(t *testing.T, limit int) (int, error) {
			tr, _ := newTestTree(t, files)
			start := time.Now()
			calls := 0
			tr.now = func() time.Time {
				calls++
				if limit >= 0 && calls > limit {
					return start.Add(time.Hour)
				}
				return start
			}
			tr.deadline = start.Add(time.Minute)
			inv, err := tr.inventory()
			if err != nil {
				return 0, err
			}
			if limit < 0 {
				if _, err := tr.readSources(inv); err != nil {
					return 0, err
				}
				return calls, nil
			}
			_, err = tr.analyzeInventory(inv)
			return calls, err
		}
		before, err := count(t, -1)
		if err != nil {
			t.Fatal(err)
		}
		// inventory と readSources の間は、期限内。その直後 (解析の最初) に、期限を過ぎる。
		if _, err := count(t, before); !errors.Is(err, errBudget) {
			t.Errorf("解析の途中で期限を過ぎたら、errBudget にすべき: %v", err)
		}
	})

	t.Run("時間", func(t *testing.T) {
		tr, _ := newTestTree(t, files)
		start := time.Now()
		tr.now = func() time.Time { return start }
		tr.deadline = start.Add(time.Second)
		if _, err := tr.readFile("a.go"); err != nil {
			t.Fatal(err)
		}
		tr.now = func() time.Time { return start.Add(2 * time.Second) }
		if _, err := tr.readFile("b.go"); !errors.Is(err, errBudget) {
			t.Errorf("期限後は errBudget にすべき: %v", err)
		}
		if _, err := tr.listDir("."); !errors.Is(err, errBudget) {
			t.Errorf("期限後は listDir も errBudget にすべき: %v", err)
		}
		if err := tr.writeFile("x.md", nil); !errors.Is(err, errBudget) {
			t.Errorf("期限後は writeFile も errBudget にすべき: %v", err)
		}
	})
}

// TestReadFileAndListDir は、通常の読み取りが、内容をそのまま返すことを確認する (安全策が、正常系を壊さない)。
func TestReadFileAndListDir(t *testing.T) {
	tr, _ := newTestTree(t, map[string]string{"d/a.go": "package a\n", "d/sub/b.go": "package b\n"})
	got, err := tr.readFile("d/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package a\n" {
		t.Errorf("readFile = %q", got)
	}
	ents, err := tr.listDir("d")
	if err != nil {
		t.Fatal(err)
	}
	want := []dirEntry{{"a.go", entRegular}, {"sub", entDir}}
	if !reflect.DeepEqual(ents, want) {
		t.Errorf("listDir = %+v, want %+v", ents, want)
	}
	if _, err := tr.readFile("nope.go"); err == nil {
		t.Error("無いファイルは error")
	}
	if _, err := tr.readFile("d"); err == nil {
		t.Error("ディレクトリは読めない")
	}
	if _, err := tr.listDir("d/a.go"); err == nil {
		t.Error("ファイルは、ディレクトリとして辿れない")
	}
}

// TestRootBoundary は、root の外を指す名前が拒否され、外側が変わらないことを確認する。
func TestRootBoundary(t *testing.T) {
	outer := t.TempDir()
	writeTree(t, outer, map[string]string{"secret.txt": "外側\n", "repo/in.go": "package in\n"})
	r := openTestRoot(t, filepath.Join(outer, "repo"))
	tr := newTree(r, defaultLimits)

	for _, name := range []string{"../secret.txt", "a/../../secret.txt", "/etc/passwd"} {
		if _, err := tr.readFile(name); err == nil {
			t.Errorf("readFile(%q) は error にすべき", name)
		}
		if err := tr.writeFile(name+".new", []byte("x")); err == nil {
			t.Errorf("writeFile(%q) は error にすべき", name+".new")
		}
	}
	if err := tr.writeFile("../evil.md", []byte("x")); err == nil {
		t.Error("root の外への書き込みは error にすべき")
	}
	if _, err := os.Lstat(filepath.Join(outer, "evil.md")); err == nil {
		t.Error("root の外に、ファイルができた")
	}
	if b, _ := os.ReadFile(filepath.Join(outer, "secret.txt")); string(b) != "外側\n" {
		t.Errorf("外側が変わった: %q", b)
	}
}

func TestWriteFile(t *testing.T) {
	tr, dir := newTestTree(t, nil)
	if err := tr.writeFile("docs/reference/sub/x.md", []byte("一回目\n")); err != nil {
		t.Fatal(err)
	}
	if err := tr.writeFile("docs/reference/sub/x.md", []byte("二\n")); err != nil { // 切り詰めて上書き
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "docs", "reference", "sub", "x.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "二\n" {
		t.Errorf("中身 = %q, want %q (O_TRUNC 相当)", got, "二\n")
	}
	info, err := os.Stat(filepath.Join(dir, "docs", "reference", "sub", "x.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o600 != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}
	if err := tr.remove("docs/reference/sub/x.md"); err != nil {
		t.Fatal(err)
	}
	if err := tr.remove("docs/reference/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "docs", "reference", "sub")); err == nil {
		t.Error("remove したのに残っている")
	}
}

func TestListReference(t *testing.T) {
	t.Run("無ければ空", func(t *testing.T) {
		tr, _ := newTestTree(t, map[string]string{"docs/adr/README.md": "x\n"})
		got, err := tr.listReference()
		if err != nil || len(got) != 0 {
			t.Errorf("listReference = %v, %v", got, err)
		}
	})
	t.Run("docs も無ければ空", func(t *testing.T) {
		tr, _ := newTestTree(t, map[string]string{"README.md": "x\n"})
		got, err := tr.listReference()
		if err != nil || len(got) != 0 {
			t.Errorf("listReference = %v, %v", got, err)
		}
	})
	t.Run("すべての項目を、名前の順に返す (名前による除外は無い)", func(t *testing.T) {
		tr, _ := newTestTree(t, map[string]string{
			"docs/reference/README.md":        "x\n",
			"docs/reference/core.md":          "x\n",
			"docs/reference/sandbox/bwrap.md": "x\n",
			"docs/reference/.hidden":          "隠しファイルも数える\n",
			"docs/reference/testdata/t.md":    "testdata も数える\n",
			"docs/reference/日本語.txt":          "path の文字は、ここでは問わない (余剰として報告される)\n",
		})
		got, err := tr.listReference()
		if err != nil {
			t.Fatal(err)
		}
		want := []refEntry{
			{"docs/reference/.hidden", false},
			{"docs/reference/README.md", false},
			{"docs/reference/core.md", false},
			{"docs/reference/sandbox", true},
			{"docs/reference/sandbox/bwrap.md", false},
			{"docs/reference/testdata", true},
			{"docs/reference/testdata/t.md", false},
			{"docs/reference/日本語.txt", false},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("listReference =\n  %+v\nwant\n  %+v", got, want)
		}
	})
	t.Run("reference がディレクトリではない", func(t *testing.T) {
		tr, _ := newTestTree(t, map[string]string{"docs/reference": "ファイル\n"})
		if _, err := tr.listReference(); err == nil {
			t.Error("error にすべき")
		}
	})
	t.Run("深さの上限", func(t *testing.T) {
		lim := defaultLimits
		lim.MaxDepth = 3 // docs/reference (2 段) の下に 1 段だけ
		tr, _ := newTestTreeLimits(t, map[string]string{"docs/reference/a/b/x.md": "x\n"}, lim)
		if _, err := tr.listReference(); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %v", err)
		}
	})
}

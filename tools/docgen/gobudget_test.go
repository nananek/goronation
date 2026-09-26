package main

import (
	"errors"
	"strings"
	"testing"
)

// 攻撃者視点レビュー (2d63f6d) の finding 7: 予算 (64 MiB) の中でも、構文解析のメモリは、入力の数十倍になる
// (最悪の形の 63 MiB で、ピーク RSS 3.6 GB)。構文解析する .go の合計に、別の上限 (limits.MaxGoBytes) を設ける。

// goFile は、ちょうど n バイトの、package core の .go を返す (n は、"package core\n" + "//" + "\n" より大きい)。
func goFile(n int) string {
	head, tail := "package core\n//", "\n"
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

// TestGoBytesBudget は、構文解析する .go の合計が、上限ちょうどは通り、1 バイト超えたら errBudget になることを確認する。
// _test.go と、. か _ で始まる名前の .go は、構文解析しないので、数えない (読むだけ)。
func TestGoBytesBudget(t *testing.T) {
	const limit = 1000
	lim := defaultLimits
	lim.MaxGoBytes = limit
	run := func(t *testing.T, files map[string]string) error {
		t.Helper()
		files["core/doc.go"] = "// Package core は、テスト。\n" + files["core/doc.go"]
		tr, _ := newTestTreeLimits(t, scaffold(files), lim)
		_, err := tr.analyze()
		return err
	}
	docLen := len("// Package core は、テスト。\n")

	t.Run("ちょうど上限 (複数のファイルの合計)", func(t *testing.T) {
		// doc.go (前置きを含めて docLen + goFile) と other.go の合計が、ちょうど limit。
		first := goFile(limit / 2)
		second := goFile(limit - docLen - len(first))
		if err := run(t, map[string]string{"core/doc.go": first, "core/other.go": second}); err != nil {
			t.Errorf("合計がちょうど上限のとき、通すべき: %v", err)
		}
	})
	t.Run("1 バイト超", func(t *testing.T) {
		first := goFile(limit / 2)
		second := goFile(limit - docLen - len(first) + 1)
		if err := run(t, map[string]string{"core/doc.go": first, "core/other.go": second}); !errors.Is(err, errBudget) {
			t.Errorf("errBudget にすべき: %v", err)
		}
	})
	t.Run("複数の package の合計", func(t *testing.T) {
		// 1 つの package では収まるが、package をまたいで合計すると超える (package ごとの上限ではない)。
		files := map[string]string{
			"core/doc.go":   goFile(limit/2 - docLen),
			"core/a/doc.go": "// Package a は、テスト。\n" + strings.Replace(goFile(limit/2), "package core", "package a", 1),
			"core/b/doc.go": "// Package b は、テスト。\n" + strings.Replace(goFile(limit/2), "package core", "package b", 1),
		}
		if err := run(t, files); !errors.Is(err, errBudget) {
			t.Errorf("package をまたいだ合計が上限を超えたら、errBudget にすべき: %v", err)
		}
	})
	t.Run("_test.go・. や _ で始まる名前は、数えない", func(t *testing.T) {
		files := map[string]string{
			"core/doc.go":       goFile(100),
			"core/big_test.go":  goFile(5 * limit),
			"core/_big.go":      goFile(5 * limit),
			"core/.big.go":      goFile(5 * limit),
			"core/big_test2.go": goFile(50),
		}
		if err := run(t, files); err != nil {
			t.Errorf("構文解析しない .go は、数えない: %v", err)
		}
	})
}

// TestDefaultGoBytes は、既定の上限が、最悪の形の入力で、ピーク RSS 約 1 GB を超えない値であることを確認する。
// 実測 (二項式 a+a, を並べた 1 MiB 弱の package): 12 MiB で約 0.9 GB、16 MiB で約 1.2 GB (入力 1 MiB あたり約 76 MiB)。
// 値を上げるときは、同じ形の入力で測り直して、doc.go の記述と、PR の説明も直す。
func TestDefaultGoBytes(t *testing.T) {
	if got, max := defaultLimits.MaxGoBytes, int64(12<<20); got > max {
		t.Errorf("MaxGoBytes = %d > %d: 最悪の入力で、ピーク RSS が約 1 GB を超える。測り直して、ここと doc.go を直す", got, max)
	}
	if defaultLimits.MaxGoBytes <= 0 || defaultLimits.MaxGoBytes >= defaultLimits.MaxTotalBytes {
		t.Errorf("MaxGoBytes = %d: MaxTotalBytes (%d) より小さい正の値にする", defaultLimits.MaxGoBytes, defaultLimits.MaxTotalBytes)
	}
}

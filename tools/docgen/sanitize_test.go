package main

import (
	"strings"
	"testing"
	"unicode"
)

func TestCheckPath(t *testing.T) {
	ok := []string{"README.md", "docs/adr/0001-repository-layout.md", "tools/docgen", "a_b/c.d-e", "github.com/nananek/goronation/core"}
	for _, s := range ok {
		if err := checkPath(s); err != nil {
			t.Errorf("checkPath(%q) = %v, want nil", s, err)
		}
	}
	bad := map[string]string{
		"空":          "",
		"空白":         "a b.go",
		"日本語":        "docs/日本語.md",
		"ESC":        "a\x1bb.go",
		"改行":         "a\nb.go",
		"NUL":        "a\x00b.go",
		"バッククォート":    "a`b.go",
		"括弧":         "a(b).go",
		"山括弧":        "a<b>.go",
		"バックスラッシュ":   `a\b.go`,
		"コロン":        "a:b.go",
		"先頭の /":      "/etc/passwd",
		"末尾の /":      "a/",
		"空の要素":       "a//b",
		".. の要素":     "a/../b",
		".. だけ":      "..",
		". の要素":      "a/./b",
		"双方向制御 RLO":  "a\u202eb.go",
		"不正な UTF-8":  "a\xffb.go",
		"全角スラッシュ":    "a／b.go",
		"Unicode 英数": "café.go",
	}
	for name, s := range bad {
		if err := checkPath(s); err == nil {
			t.Errorf("%s: checkPath(%q) = nil, want error", name, s)
		}
		if _, err := normalize(kindPath, s); err == nil {
			t.Errorf("%s: normalize(kindPath, %q) = nil, want error", name, s)
		}
	}
}

func TestForbiddenRune(t *testing.T) {
	forbidden := []rune{0x00, 0x01, 0x07, 0x08, 0x0b, 0x0c, '\r', 0x1b, 0x1f, 0x7f, 0x80, 0x85, 0x9f,
		0x061C, 0x200E, 0x200F, 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069, 0x2028, 0x2029,
		// 書式制御文字 (Cf): 見えない。ゼロ幅・soft hyphen・word joiner・BOM・タグ文字 (ASCII を隠せる)。
		0x00AD, 0x200B, 0x200C, 0x200D, 0x2060, 0x2064, 0xFEFF, 0xE0001, 0xE0041, 0xE007F,
		// Cf ではない、見えない文字 (Default_Ignorable_Code_Point): CGJ・Hangul filler・Khmer・Mongolian の異体字選択・未割当。
		0x034F, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x180B, 0x180C, 0x180D, 0x180F, 0x2065, 0x3164, 0xFFA0, 0xFFF0, 0xFFF8,
		0xE0000, 0xE0002, 0xE001F, 0xE0080, 0xE00FF, 0xE01F0, 0xE0FFF, // E00FF・E01F0 は、IVS (E0100〜E01EF) のすぐ外
		0x2800} // 点字の空白 (DI ではないが、見た目が空白)
	for _, r := range forbidden {
		if !forbiddenRune(r) {
			t.Errorf("forbiddenRune(U+%04X) = false, want true", r)
		}
	}
	allowed := []rune{'\n', '\t', ' ', 'a', '~', 0xa0, 0xa1, '日', 'ー', '。', 0x2010, 0x2027, 0x2030, 0x3000, 0xFF01, 0x1F600,
		// 結合文字 (Mn)・異体字セレクタ: 正当な文書に出る。Cf ではないので、禁止しない (Cf だけを禁止する境界)。
		0x0301, 0x3099, 0x309A, // アクセント・濁点 (NFD の日本語)
		0xFE0F,          // 絵文字の異体字セレクタ (VS16。⚠ の後ろに付く)
		0xE0100,         // 漢字の異体字セレクタ (IVS の VS17。葛 の後ろに付く)
		0x20E3, 0x1F3FB, // 囲みキーキャップ・肌の色の修飾子
		0xFE00, 0xE01EF, // 異体字セレクタの範囲の両端
		0xFE10, 0xFDFF, // 異体字セレクタのすぐ外 (見える句読点・未割当で DI でないもの)
		0x1D159, 0xFFFC, 0x2003, // 見える文字 (音楽記号・オブジェクト置換文字・em space)
	}
	for _, r := range allowed {
		if forbiddenRune(r) {
			t.Errorf("forbiddenRune(U+%04X) = true, want false", r)
		}
	}
}

// TestDefaultIgnorableIsForbidden は、見えない文字 (Unicode の Default_Ignorable_Code_Point) を、個別の列挙ではなく、性質で
// 禁止することを確認する (攻撃者視点レビュー 2d63f6d の finding 2)。forbiddenRune 単体で通るのは、異体字セレクタ
// (U+FE00〜FE0F・U+E0100〜E01EF。絵文字と漢字の異体字に使う) だけで、置き場所は runeError が見る
// (variation_selector_test.go。連続と基底の無い単独は error。交互は限界として、doc.go に書いてある)。
func TestDefaultIgnorableIsForbidden(t *testing.T) {
	// レビューが実測した、通っていた文字と、その仲間 (Cf でない DI・未割当の DI・点字の空白) は、どの入口でも error。
	for _, r := range []rune{0x034F, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x180B, 0x180C, 0x180D, 0x180F,
		0x2065, 0x2800, 0x3164, 0xFFA0, 0xFFF0, 0xFFF8, 0xE0000, 0xE0002, 0xE001F, 0xE0080, 0xE00FF, 0xE01F0, 0xE0FFF} {
		s := "a" + string(r) + "b"
		if !forbiddenRune(r) {
			t.Errorf("forbiddenRune(U+%04X) = false, want true", r)
		}
		if _, err := normalize(kindText, s); err == nil {
			t.Errorf("normalize(kindText, U+%04X) が error にならない", r)
		}
		if err := checkSource("x/a.go", []byte("package a\n// "+s+"\n")); err == nil {
			t.Errorf("checkSource が、コメントの中の U+%04X を見逃した", r)
		}
		// 端から端まで: doc comment に入れると、生成が error になる。
		if page, err := corePage(t, docComment("Package core は、テスト。", "", "前"+string(r)+"後。")); err == nil {
			t.Errorf("U+%04X を含む doc comment の生成が、error にならない:\n%s", r, page)
		}
	}

	// 全 code point: Default_Ignorable の材料 (Other_Default_Ignorable_Code_Point・Cf・Variation_Selector) は、異体字セレクタの
	// 256 個 (U+FE00〜FE0F・U+E0100〜E01EF) を除いて、すべて禁止する。例外が広がる・狭まる変更で赤になる。
	allowed := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Variation_Selector, r) {
			continue
		}
		variation := 0xFE00 <= r && r <= 0xFE0F || 0xE0100 <= r && r <= 0xE01EF
		if variation {
			allowed++
		}
		if forbiddenRune(r) == variation {
			t.Errorf("forbiddenRune(U+%04X) = %v, want %v (異体字セレクタだけが通る)", r, forbiddenRune(r), !variation)
		}
	}
	if allowed != 256 {
		t.Errorf("通る異体字セレクタが %d 個, want 256 (U+FE00〜FE0F の 16 個と、U+E0100〜E01EF の 240 個)", allowed)
	}
}

// TestJapaneseIsNotForbidden は、日本語の文書に出る主な範囲の文字が、1 文字も禁止されないことを確認する (偽陽性の歯止め)。
func TestJapaneseIsNotForbidden(t *testing.T) {
	for _, rg := range [][2]rune{
		{0x20, 0x7E},       // ASCII
		{0x3000, 0x303F},   // 句読点・記号・全角空白
		{0x3041, 0x3096},   // ひらがな
		{0x3099, 0x309F},   // 濁点・半濁点 (結合文字) ・ゝ
		{0x30A0, 0x30FF},   // カタカナ
		{0x31F0, 0x31FF},   // カタカナ拡張
		{0x4E00, 0x9FFF},   // CJK 統合漢字
		{0xFF01, 0xFF9F},   // 全角・半角のカナ (U+FFA0 の Hangul filler は含まない)
		{0x2010, 0x2027},   // 記号・約物
		{0x2190, 0x21FF},   // 矢印
		{0x2600, 0x26FF},   // 記号 (⚠ など)
		{0x1F300, 0x1F5FF}, // 絵文字 (ZWJ を含まない)
	} {
		for r := rg[0]; r <= rg[1]; r++ {
			if forbiddenRune(r) {
				t.Errorf("forbiddenRune(U+%04X) = true: 日本語の文書に出る範囲 %U〜%U の文字を、誤って禁止した", r, rg[0], rg[1])
			}
		}
	}
}

func TestEscapeDiag(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ふつうの文 (ASCII と日本語)。", "ふつうの文 (ASCII と日本語)。"},
		{"a\x1b[31mred\x1b[0m", `a\x1b[31mred\x1b[0m`},
		{"BEL\x07 BS\x08 NUL\x00", `BEL\x07 BS\x08 NUL\x00`},
		{"line1\nline2", `line1\nline2`},
		{"tab\there", `tab\there`},
		{"cr\rlf", `cr\x0dlf`},
		{"C1\u0085", `C1\x85`},
		{"rlo\u202e", `rlo\u202e`},
		{"sep\u2028", `sep\u2028`},
		{"zw\u200bsp", `zw\u200bsp`},
		{"bom\ufeff", `bom\ufeff`},
		{"tag\U000e0041", `tag\U000e0041`},
		{"bad\xffutf8", `bad\xffutf8`},
	}
	for _, tc := range cases {
		got, err := normalize(kindDiag, tc.in)
		if err != nil {
			t.Errorf("normalize(kindDiag, %q) = error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalize(kindDiag, %q) = %q, want %q", tc.in, got, tc.want)
		}
		for _, r := range got {
			if forbiddenRune(r) || r == '\n' {
				t.Errorf("normalize(kindDiag, %q) = %q に、U+%04X が残った", tc.in, got, r)
			}
		}
		if strings.ContainsRune(got, 0x1b) {
			t.Errorf("ESC が残った: %q", got)
		}
	}
}

func TestNormalizeUnknownKind(t *testing.T) {
	if _, err := normalize(kind(-1), "x"); err == nil {
		t.Error("未知の種類は error にすべき (黙って素通しにしない)")
	}
}

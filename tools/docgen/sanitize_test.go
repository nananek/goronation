package main

import (
	"strings"
	"testing"
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
		0x00AD, 0x200B, 0x200C, 0x200D, 0x2060, 0x2064, 0xFEFF, 0xE0001, 0xE0041, 0xE007F}
	for _, r := range forbidden {
		if !forbiddenRune(r) {
			t.Errorf("forbiddenRune(U+%04X) = false, want true", r)
		}
	}
	allowed := []rune{'\n', '\t', ' ', 'a', '~', 0xa0, 0xa1, '日', 'ー', '。', 0x2010, 0x2027, 0x2030, 0x3000, 0xFF01, 0x1F600}
	for _, r := range allowed {
		if forbiddenRune(r) {
			t.Errorf("forbiddenRune(U+%04X) = true, want false", r)
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

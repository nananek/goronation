package main

import (
	"math/rand"
	"strings"
	"testing"
)

func TestNormalizeText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ふつうの文。", "ふつうの文。"},
		{`a * b _ c ` + "`d`" + ` < > & | # $ ~ [ ] \ z`, `a \* b \_ c \` + "`d\\`" + ` \< \> \& \| \# \$ \~ \[ \] \\ z`},
		{"- 先頭のハイフン", `\- 先頭のハイフン`},
		{"+ 先頭のプラス", `\+ 先頭のプラス`},
		{"  - 空白の後のハイフン", `  \- 空白の後のハイフン`},
		{"1. 先頭の番号", `1\. 先頭の番号`},
		{"12) 先頭の番号", `12\) 先頭の番号`},
		{"1 は番号ではない", "1 は番号ではない"},
		{"1.5 は番号ではない", `1\.5 は番号ではない`}, // 余分にエスケープしても、描画は同じ
		{"途中の - と + と 1. は、そのまま", "途中の - と + と 1. は、そのまま"},
		{"a\tb", "a b"},
		{"a\nb", "a b"},
		{"日本\n語", "日本語"},
		{"日本語。\n次の文", "日本語。次の文"},
		{"os/exec の\n実行", "os/exec の実行"},
		{"実行\nos/exec", "実行 os/exec"},
		{"a  \n  b", "a b"},
		{"\nb", " b"},
		{"a\n", "a "},
		{"", ""},
	}
	for _, tc := range cases {
		got, err := normalize(kindText, tc.in)
		if err != nil {
			t.Errorf("normalize(kindText, %q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalize(kindText, %q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeCell は、表のセルでは、行頭の記号をエスケープしない (セルの中では意味を持たない) ことを確認する。
func TestNormalizeCell(t *testing.T) {
	cases := []struct{ in, want string }{
		{"-", "-"},
		{"- x", "- x"},
		{"1. x", "1. x"},
		{"a | b", `a \| b`},
		{"* _ `", "\\* \\_ \\`"},
	}
	for _, tc := range cases {
		if got, err := normalize(kindCell, tc.in); err != nil || got != tc.want {
			t.Errorf("normalize(kindCell, %q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
}

// unescape は、escapeMarkdown の出力から、エスケープを外す。エスケープされていない特殊文字があれば、ok = false。
func unescape(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			i++
			if i >= len(s) {
				return "", false
			}
			b.WriteByte(s[i])
		case strings.IndexByte(markdownSpecials, c) >= 0:
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// TestEscapeProperty は、特殊文字を多く含む入力で、(1) 出力に、エスケープされていない特殊文字が無く、
// (2) エスケープを外すと入力に戻る (情報を落とさない) ことを確認する。
func TestEscapeProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"\\", "`", "*", "_", "[", "]", "<", ">", "&", "|", "~", "$", "#", "-", "+", ".", ")", "1", "9", " ", "a", "Z", "日", "。", "(", "!", "="}
	for range 3000 {
		var b strings.Builder
		for range rng.Intn(12) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		in := b.String()
		for _, k := range []kind{kindText, kindCell} {
			got, err := normalize(k, in)
			if err != nil {
				t.Fatalf("normalize(%d, %q): %v", k, in, err)
			}
			back, ok := unescape(got)
			if !ok {
				t.Fatalf("normalize(%d, %q) = %q に、エスケープされていない特殊文字がある", k, in, got)
			}
			if back != in {
				t.Fatalf("normalize(%d, %q) = %q: エスケープを外すと %q (入力に戻らない)", k, in, got, back)
			}
		}
	}
}

func TestCodeSpan(t *testing.T) {
	cases := []struct{ in, want string }{
		{"x", "`x`"},
		{"a b", "`a b`"},
		{"a`b", "``a`b``"},
		{"`a", "`` `a ``"},
		{"a`", "`` a` ``"},
		{"a``b", "```a``b```"},
		{"a\nb\tc", "`a b c`"},
		{"", "``"},
	}
	for _, tc := range cases {
		got, err := normalize(kindSpan, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalize(kindSpan, %q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
	// 内容の連続バッククォート数より、囲みが長い。
	for n := 0; n <= 10; n++ {
		in := "a" + strings.Repeat("`", n) + "b"
		got, _ := normalize(kindSpan, in)
		fence := got[:len(got)-len(strings.TrimLeft(got, "`"))]
		if len(fence) != n+1 {
			t.Errorf("連続 %d 個: 囲みが %d 個", n, len(fence))
		}
		if !strings.HasSuffix(got, fence) {
			t.Errorf("閉じる囲みが無い: %q", got)
		}
	}
}

func TestCodeBlockFence(t *testing.T) {
	for n := 0; n <= 12; n++ {
		content := "before\n" + strings.Repeat("`", n) + "\nafter"
		for _, k := range []kind{kindCodeBlock, kindGoBlock} {
			got, err := normalize(k, content)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(got, "\n")
			open, closing := lines[0], lines[len(lines)-1]
			fence := strings.TrimRight(open, "gio")
			if want := max(3, n+1); len(fence) != want {
				t.Errorf("連続 %d 個: 開く囲み %q は、%d 個であるべき", n, open, want)
			}
			if closing != fence {
				t.Errorf("閉じる囲み %q が、開く囲み %q と違う", closing, fence)
			}
			// 内容の行に、囲みを閉じられるものが無い (囲み以上の長さのバッククォートだけの行)。
			for _, ln := range lines[1 : len(lines)-1] {
				if strings.Trim(ln, "`") == "" && len(ln) >= len(fence) {
					t.Errorf("内容の行 %q が、囲み %q を閉じる", ln, fence)
				}
			}
		}
	}
	if got, _ := normalize(kindGoBlock, "x\n"); got != "```go\nx\n```" {
		t.Errorf("Go のコードブロック = %q", got)
	}
	if got, _ := normalize(kindCodeBlock, "x\n\n\n"); got != "```\nx\n```" {
		t.Errorf("末尾の空行は、除く: %q", got)
	}
}

func TestCheckURL(t *testing.T) {
	ok := []string{
		"https://example.com", "http://example.com/a/b?c=d&e=f#g", "HTTPS://EXAMPLE.COM/", "https://example.com:8080/x",
		"docs/adr/0001-x.md", "../a.md", "./a.md", "a.md#frag", "#frag", "../../README.md", "x", "https://example.com/a%20b",
	}
	for _, u := range ok {
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"", "javascript:alert(1)", "JAVASCRIPT:alert(1)", " javascript:alert(1)", "java\tscript:alert(1)", "data:text/html,x", "vbscript:x",
		"file:///etc/passwd", "ftp://example.com", "mailto:a@example.com", "tel:123", "ws://example.com", "//evil.example", "///evil",
		"https://user@example.com/", "https://user:pw@example.com/", "https:///x", "http://", "https:",
		"a b", "a\"b", "a'b" + "\\", "a`b", "a<b", "a>b", "a(b", "a)b", "a[b", "a]b", "a{b", "a\\b", "a|b", "a^b",
		"https://example.com/日本語", "https://example.com/\x00", "https://example.com/\n", "%zz", "https://[::1]/",
		"java%73cript:alert(1)", "javascript%3Aalert(1)",
	}
	for _, u := range bad {
		if err := checkURL(u); err == nil {
			// "javascript%3Aalert(1)" は ( を含むので error。%3A だけなら相対の path (下)。
			t.Errorf("checkURL(%q) = nil, want error", u)
		}
		if _, err := normalize(kindURL, u); err == nil {
			t.Errorf("normalize(kindURL, %q) = nil, want error", u)
		}
	}
	// %3A を含むだけの相対 path は、scheme にならない (先頭の要素に : が無い)。
	if err := checkURL("javascript%3Aalert"); err != nil {
		t.Errorf("checkURL(javascript%%3Aalert) = %v: scheme にならない相対 path", err)
	}
}

func TestNormalizeIdent(t *testing.T) {
	for _, s := range []string{"Foo", "foo_bar", "日本語", "X1", "_x"} {
		if got, err := normalize(kindIdent, s); err != nil || got != s {
			t.Errorf("normalize(kindIdent, %q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "1x", "a b", "a.b", "a-b", "a\"b", "<b>", "a\x1b", "func", "a\u202eb"} {
		if _, err := normalize(kindIdent, s); err == nil {
			t.Errorf("normalize(kindIdent, %q) は error にすべき", s)
		}
	}
}

// TestNormalizeRejectsForbidden は、どの種類 (kindDiag 以外) も、禁止する文字と不正な UTF-8 を、error にすることを確認する。
func TestNormalizeRejectsForbidden(t *testing.T) {
	kinds := map[string]kind{"path": kindPath, "text": kindText, "cell": kindCell, "span": kindSpan, "code": kindCodeBlock, "go": kindGoBlock, "ident": kindIdent, "url": kindURL, "document": kindDocument}
	bad := []string{"a\x1bb", "a\x07b", "a\x08b", "a\x00b", "a\x7fb", "a\u0085b", "a\u202eb", "a\u2028b", "a\u2069b", "a\rb", "a\xffb",
		"a\u200bb", "a\u200db", "a\u2060b", "a\ufeffb", "a\U000e0041b"}
	for name, k := range kinds {
		for _, s := range bad {
			if got, err := normalize(k, s); err == nil {
				t.Errorf("normalize(%s, %q) = %q: error にすべき", name, s, got)
			}
		}
	}
	// 改行とタブは、許す種類 (文章・コード・文書) では通る。
	for _, k := range []kind{kindText, kindCell, kindSpan, kindCodeBlock, kindGoBlock} {
		if _, err := normalize(k, "a\tb\nc"); err != nil {
			t.Errorf("kind %d: 改行とタブは通すべき: %v", k, err)
		}
	}
	if got, err := normalize(kindDocument, "a\tb\nc\n"); err != nil || got != "a\tb\nc\n" {
		t.Errorf("kindDocument = %q, %v", got, err)
	}
	if _, err := normalize(kindDocument, "改行で終わらない"); err == nil {
		t.Error("kindDocument は、改行で終わらない文書を error にすべき")
	}
	if got, err := normalize(kindDocument, ""); err != nil || got != "" {
		t.Errorf("空の文書: %q, %v", got, err)
	}
}

func TestCheckSource(t *testing.T) {
	if err := checkSource("a.go", []byte("package a\n\n// 日本語のコメント。\tタブもよい。\n")); err != nil {
		t.Errorf("正常な source: %v", err)
	}
	cases := map[string]string{
		"ESC":         "package a\n// x\x1b[31m\n",
		"CR":          "package a\r\n",
		"C1":          "package a\n// x\u0085y\n",
		"RLO":         "package a\n// x\u202ey\n",
		"NUL":         "package a\n// x\x00y\n",
		"invalid":     "package a\n// x\xffy\n",
		"3 行目":        "package a\n\n// FF\x0c\n",
		"行末の LS":      "package a\n// x\u2028\n",
		"DEL":         "package a\n// x\x7f\n",
		"文字列の中の BEL":  "package a\nconst c = \"a\x07b\"\n",
		"ZWSP":        "package a\n// x\u200by\n",
		"ZWJ":         "package a\n// x\u200dy\n",
		"文字列の中の ZWSP": "package a\nconst c = \"a\u200bb\"\n",
		"先頭の BOM":     "\ufeffpackage a\n",
		"タグ文字":        "package a\n// x\U000e0041y\n",
	}
	for name, src := range cases {
		err := checkSource("x/a.go", []byte(src))
		if err == nil {
			t.Errorf("%s: error にすべき", name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "x/a.go:") {
			t.Errorf("%s: error に、ファイル名と行が無い: %v", name, err)
		}
	}
	if err := checkSource("a.go", []byte("package a\n\n// FF\x0c\n")); err == nil || !strings.Contains(err.Error(), "a.go:3:") {
		t.Errorf("行番号が違う: %v", err)
	}
	// CRLF は、直し方 (LF にする) が分かる専用の文にする。
	if err := checkSource("a.md", []byte("x\r\n")); err == nil || !strings.Contains(err.Error(), "CR") || !strings.Contains(err.Error(), "LF") {
		t.Errorf("CR の error 文: %v", err)
	}
}

func TestIsCJK(t *testing.T) {
	for _, r := range "日本語ひらがなカタカナ、。「」（）！ＡＢ" {
		if !isCJK(r) {
			t.Errorf("isCJK(%q) = false", r)
		}
	}
	for _, r := range "aZ09 .,-_/é" {
		if isCJK(r) {
			t.Errorf("isCJK(%q) = true", r)
		}
	}
}

package github

import (
	"strings"
	"testing"
	"unicode"
)

// oracleForbidden は、直前が基底の文字 ('a') のときの、題・本文に入れられない文字の判定。
// forbiddenRune とは別の書き方で、Unicode の一般カテゴリと性質 (Bidi_Control・Join_Control など) だけで表す。
func oracleForbidden(r rune, multiline bool) bool {
	if multiline && (r == '\n' || r == '\r' || r == '\t') {
		return false
	}
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) || unicode.In(r, unicode.Bidi_Control, unicode.Join_Control) || r == 0x2800 {
		return true
	}
	if unicode.Is(unicode.Variation_Selector, r) {
		return !(r >= 0xfe00 && r <= 0xfe0f || r >= 0xe0100 && r <= 0xe01ef) // 異体字セレクタは、基底の文字の後なら通す
	}
	return unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
}

// TestForbiddenRuneEveryCodePoint は、全ての符号位置 (サロゲートを除く) で、forbiddenRune が oracleForbidden と一致することを確かめる。
// 実装と同じ集合を写すのでなく、性質から求めた判定と、全数で照合する。
func TestForbiddenRuneEveryCodePoint(t *testing.T) {
	var forbidden, allowed, bad int
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		for _, multiline := range []bool{false, true} {
			got, want := forbiddenRune('a', r, multiline), oracleForbidden(r, multiline)
			if got != want && bad < 20 {
				bad++
				t.Errorf("U+%04X (multiline=%v): forbiddenRune = %v, 期待 %v", r, multiline, got, want)
			}
			if !multiline {
				if got {
					forbidden++
				} else {
					allowed++
				}
			}
		}
	}
	// 判定が全て true・全て false に偏っていない (oracle の取り違えの検出)。
	if forbidden < 2000 || allowed < 1_000_000 {
		t.Errorf("断る %d 個・通す %d 個。偏っている", forbidden, allowed)
	}
}

// TestForbiddenRuneVariationSelectorPlacement は、異体字セレクタが、基底の文字の後だけで通ることを確かめる。
func TestForbiddenRuneVariationSelectorPlacement(t *testing.T) {
	selectors := []rune{0xfe00, 0xfe0e, 0xfe0f, 0xe0100, 0xe01ef}
	for _, vs := range selectors {
		for prev, want := range map[rune]bool{
			0: true, ' ': true, '\n': true, '\t': true, 0x3000: true, 0xfe0f: true, 0xe0100: true, // 断る
			'a': false, 0x845b: false, 0x2764: false, 0x0301: false, // 通す (文字・漢字・記号・結合文字)
		} {
			if got := forbiddenRune(prev, vs, true); got != want {
				t.Errorf("U+%04X の直前が U+%04X: forbiddenRune = %v, 期待 %v", vs, prev, got, want)
			}
		}
	}
	// 直前が異体字セレクタ以外の、基底でない文字でも、範囲外の異体字選択 (Mongolian) は、いつでも断る。
	for _, vs := range []rune{0x180b, 0x180c, 0x180d, 0x180f} {
		if !forbiddenRune('a', vs, true) {
			t.Errorf("Mongolian の異体字選択 U+%04X を通した", vs)
		}
	}
}

// TestParsePullTextExamples は、通す・断る文字の例を、title と body の両方で固定する。
func TestParsePullTextExamples(t *testing.T) {
	pp := policyFor(t, repoOR)
	r := func(cs ...rune) string { return string(cs) }
	accept := map[string]string{
		"japanese":       "日本語の題、「括弧」。ＡＢＣ　全角",
		"latin-accents":  "caf" + r(0xe9) + " na" + r('i', 0x308) + "ve",
		"combining":      "e" + r(0x301),
		"emoji":          "fix " + r(0x1f600),
		"emoji-vs16":     "heart " + r(0x2764, 0xfe0f),
		"ivs":            "葛" + r(0xe0100) + "城",
		"nbsp":           "a" + r(0xa0) + "b",
		"symbols":        "a -> b (c) [d] {e} <f> & \"g\" 'h' `i` \\j /k",
		"object-replace": "a" + r(0xfffc) + "b", // Cf ではなく So (見える記号)。tools/docgen も通す
	}
	reject := map[string]string{
		"zwsp": r(0x200b), "zwnj": r(0x200c), "zwj": r(0x200d), "zwj-emoji": r(0x1f468, 0x200d, 0x1f469), "word-joiner": r(0x2060),
		"invisible-times": r(0x2062), "soft-hyphen": r(0xad), "bom": r(0xfeff), "alm": r(0x61c), "lrm": r(0x200e), "rlm": r(0x200f),
		"rlo": r(0x202e), "lri": r(0x2066), "pdi": r(0x2069), "interlinear": r(0xfff9),
		"tag-latin-a": r(0xe0041), "tag-space": r(0xe0020), "tag-cancel": r(0xe007f), "tag-begin": r(0xe0001), "tag-e0000": r(0xe0000),
		"tag-e0002": r(0xe0002), "tag-e001f": r(0xe001f), "tag-e0080": r(0xe0080), "tag-e01f0": r(0xe01f0),
		"hangul-filler": r(0x115f), "hangul-filler-2": r(0x1160), "hangul-filler-3": r(0x3164), "halfwidth-hangul-filler": r(0xffa0),
		"cgj": r(0x34f), "khmer-inherent": r(0x17b4), "khmer-inherent-2": r(0x17b5), "mongolian-vs": r(0x180b), "mongolian-vsep": r(0x180e),
		"braille-blank": r(0x2800), "line-sep": r(0x2028), "para-sep": r(0x2029), "nel": r(0x85), "nul": r(0), "esc": r(0x1b), "del": r(0x7f),
		"tab": "\t", "lf": "\n", "cr": "\r", "unassigned-2065": r(0x2065), "fff0": r(0xfff0),
	}
	for name, s := range accept {
		for _, field := range []string{"title", "body"} {
			if _, err := pp.ParsePull(repoOR, []byte(textBody(t, field, s))); err != nil {
				t.Errorf("%s (%s): 通るはず: %v", name, field, err)
			}
		}
	}
	for name, s := range reject {
		for _, field := range []string{"title", "body"} {
			if field == "body" && (name == "tab" || name == "lf" || name == "cr") {
				continue // body は、改行とタブを入れてよい
			}
			_, err := pp.ParsePull(repoOR, []byte(textBody(t, field, "a"+s+"b")))
			if Reason(err) != CodeBadField {
				t.Errorf("%s (%s): 断るはず: Reason = %q (%v)", name, field, Reason(err), err)
			}
		}
	}
	// 異体字セレクタの置き場: 先頭・空白の後・連続は断る。
	for name, s := range map[string]string{
		"leading-vs": r(0xfe0f) + "a", "after-space": "a " + r(0xfe0f), "double-vs": "a" + r(0xfe0f, 0xfe0f),
		"after-newline": "a\n" + r(0xfe0f), "leading-ivs": r(0xe0100) + "a",
	} {
		if _, err := pp.ParsePull(repoOR, []byte(textBody(t, "body", s))); Reason(err) != CodeBadField {
			t.Errorf("%s: 断るはず: %v", name, err)
		}
	}
	// 断るときのエラーは、どの文字かを、U+XXXX で示す (エージェントが直せる)。
	_, err := pp.ParsePull(repoOR, []byte(textBody(t, "title", "a"+r(0x200d)+"b")))
	if err == nil || !strings.Contains(err.Error(), "U+200D") || !strings.Contains(err.Error(), "title") {
		t.Errorf("エラーに、項目と文字が無い: %v", err)
	}
}

func textBody(t testing.TB, field, s string) string {
	t.Helper()
	if field == "title" {
		return pullBody(t, "title", s, "head", headOf("x"))
	}
	return pullBody(t, "title", "t", "body", s, "head", headOf("x"))
}

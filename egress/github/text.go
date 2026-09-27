package github

import "unicode"

// forbiddenRune は、題・本文に入れられない文字か。prev は、直前の文字 (先頭は 0)。multiline なら、改行 (LF・CR) とタブは入れてよい。
// 方針は tools/docgen (forbiddenRune・runeError) と同じ: 見えない文字・表示の向きを変える文字で、人にも AI のレビュアーにも読めない内容を隠せるため。
//   - 制御文字 (Cc: C0・DEL・C1)・行と段落の区切り (U+2028・U+2029)・点字の空白 (U+2800。見た目が空白)。
//   - 書式制御文字 (Cf) の全て: 双方向制御 (U+061C・U+200E・U+200F・U+202A〜202E・U+2066〜2069)・ゼロ幅の文字 (ZWJ・ZWNJ を含む)・BOM・
//     ソフトハイフン・タグ文字 (U+E0001・U+E0020〜E007F)。絵文字の ZWJ での連結 (家族の絵文字など) も、書けない。
//   - Default_Ignorable_Code_Point の Cf 以外 (Hangul filler・CGJ・Khmer の U+17B4・U+17B5・Mongolian の異体字選択・未割当の U+E0000 台など)。
//     表は、Go の unicode パッケージの Other_Default_Ignorable_Code_Point と Variation_Selector を使う。
//   - 異体字セレクタ (U+FE00〜FE0F・U+E0100〜E01EF) は、基底の文字に付くときだけ (直前が、異体字セレクタ・空白・先頭なら、断る)。連続で、見えないデータを運べるため。
func forbiddenRune(prev, r rune, multiline bool) bool {
	switch {
	case multiline && (r == '\n' || r == '\r' || r == '\t'):
		return false
	case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == 0x2028, r == 0x2029, r == 0x2800:
		return true
	case isVariationSelector(r):
		return prev == 0 || isVariationSelector(prev) || unicode.IsSpace(prev)
	}
	return unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r)
}

// isVariationSelector は、異体字セレクタ U+FE00〜FE0F (絵文字) と U+E0100〜E01EF (漢字の異体字。日本語で正当に使う) か。
func isVariationSelector(r rune) bool {
	return 0xfe00 <= r && r <= 0xfe0f || 0xe0100 <= r && r <= 0xe01ef
}

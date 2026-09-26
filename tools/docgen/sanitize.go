package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// kind は、normalize に渡す文字列の種類。種類ごとに、検査か、エスケープの方針が違う。
type kind int

const (
	// kindPath は、repo 相対の path と module path。許可する文字だけ (pathChars)。
	kindPath kind = iota
	// kindDiag は、診断 (標準出力・標準エラー) の 1 行。error にせず、制御文字と改行をエスケープして返す。
	kindDiag
)

// normalize は、人間が読む出口 (生成物・診断) へ出す文字列を、必ず通す唯一の関数。
// 出口が増えても、この関数を通す (通らない出口は、sanitize_test.go の自己検査が見つける)。
//
// 制御文字などを含む入力は、除去せず error にする (正当な文書には無く、除去は入力と出力の食い違いを隠す)。
// ただし kindDiag だけは、エラー文の中の文字列を出さないわけにいかないので、エスケープして返し、error にしない。
func normalize(k kind, s string) (string, error) {
	switch k {
	case kindPath:
		return s, checkPath(s)
	case kindDiag:
		return escapeDiag(s), nil
	}
	return "", fmt.Errorf("normalize: 未知の種類 %d", int(k))
}

// forbiddenRune は、文書にも診断にも出さない文字か。改行とタブ以外の制御文字 (C0・DEL・C1)、
// 表示の向きを変える文字 (双方向制御。Trojan Source)、行・段落の区切り (U+2028・U+2029) を含む。
func forbiddenRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case unicode.IsControl(r):
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F, r == 0x2028, r == 0x2029:
		return true
	case 0x202A <= r && r <= 0x202E, 0x2066 <= r && r <= 0x2069:
		return true
	}
	return false
}

// pathChars は、path と module path に許す文字。
func pathChars(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
		r == '.' || r == '_' || r == '/' || r == '-'
}

// checkPath は、s が repo 相対の path (または module path) として、許す文字だけで書かれているかを確かめる。
// 空の要素・"." と ".." の要素・先頭の "/" も許さない (root の外や、別のものを指せるため)。
func checkPath(s string) error {
	if s == "" {
		return fmt.Errorf("path が空")
	}
	for _, r := range s {
		if !pathChars(r) {
			return fmt.Errorf("path %q に、許可しない文字 %q がある (許可: 英数字と . _ / -)", s, r)
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("path %q に、空・. ・.. の要素がある", s)
		}
	}
	return nil
}

// escapeDiag は、診断の文字列の、禁止する文字 (forbiddenRune) と改行・タブ・不正な UTF-8 を、\x..・\u.... に直す。
// 攻撃者が付けられる名前や本文が、端末の制御列や、偽の行を出力に出さないため。
func escapeDiag(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && w == 1:
			b.WriteString(fmt.Sprintf(`\x%02x`, s[i]))
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case forbiddenRune(r) && r < 0x100:
			b.WriteString(fmt.Sprintf(`\x%02x`, r))
		case forbiddenRune(r):
			b.WriteString(fmt.Sprintf(`\u%04x`, r))
		default:
			b.WriteRune(r)
		}
		i += w
	}
	return b.String()
}

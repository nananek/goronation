package main

import (
	"fmt"
	"go/token"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// kind は、normalize に渡す文字列の種類。種類ごとに、検査と、エスケープや囲みの方針が違う。
type kind int

const (
	// kindPath は、repo 相対の path と module path。許可する文字だけ (checkPath)。そのまま返す。
	kindPath kind = iota
	// kindDiag は、診断 (標準出力・標準エラー) の 1 行。error にせず、制御文字と改行をエスケープして返す。
	kindDiag
	// kindText は、行内の文章 (段落・見出し・表のセル・リスト項目)。Markdown の特殊文字をエスケープし、
	// 改行とタブを空白 (日本語どうしなら詰める) にして返す。
	kindText
	// kindCell は、表のセルの文章。kindText と同じだが、行頭の記号 (リストになる + - や番号) は、セルの中では
	// 意味を持たないので、エスケープしない。
	kindCell
	// kindSpan は、行内のコード。バッククォートの囲みまで含めて返す。
	kindSpan
	// kindCodeBlock は、コードブロックの中身。内容の連続バッククォート数 + 1 (3 以上) の fence で囲んで返す。
	kindCodeBlock
	// kindGoBlock は、Go のコードブロック (info string は go)。囲みは kindCodeBlock と同じ。
	kindGoBlock
	// kindIdent は、Go の識別子。そのまま返す。
	kindIdent
	// kindURL は、リンク先。http(s) か、相対だけ。そのまま返す。
	kindURL
	// kindDocument は、書き出す文書の全体。人間向けの出口へ出す最後の関門で、どの部品を通ったかによらず、
	// 全体を検査する。そのまま返す。
	kindDocument
)

// normalize は、人間が読む出口 (生成物・診断) へ出す文字列を、必ず通す唯一の関数。
// 生成物の部品は、種類ごとに (kindText・kindSpan・kindCodeBlock・kindGoBlock・kindIdent・kindURL・kindPath)、
// 書き出す文書の全体は kindDocument で、診断は kindDiag で、ここを通る。通らない出口は、source_test.go の
// 自己検査が見つける (標準出力・標準エラーは emit だけ、ファイルへの書き込みは、writeOutput だけ)。
//
// 制御文字などを含む入力は、除去せず error にする (正当な文書には無く、除去は、入力と出力の食い違いを隠す)。
// ただし kindDiag だけは、エラー文の中に文字列を出さないわけにいかないので、エスケープして返し、error にしない。
func normalize(k kind, s string) (string, error) {
	if k == kindDiag {
		return escapeDiag(s), nil
	}
	if err := checkRunes(s); err != nil {
		return "", err
	}
	switch k {
	case kindPath:
		return s, checkPath(s)
	case kindText:
		return escapeMarkdown(joinLines(s), true), nil
	case kindCell:
		return escapeMarkdown(joinLines(s), false), nil
	case kindSpan:
		return codeSpan(s), nil
	case kindCodeBlock:
		return codeBlock("", s), nil
	case kindGoBlock:
		return codeBlock("go", s), nil
	case kindIdent:
		if !token.IsIdentifier(s) {
			return "", fmt.Errorf("%q は Go の識別子ではない", s)
		}
		return s, nil
	case kindURL:
		return s, checkURL(s)
	case kindDocument:
		if s != "" && !strings.HasSuffix(s, "\n") {
			return "", fmt.Errorf("文書が改行で終わらない")
		}
		return s, nil
	}
	return "", fmt.Errorf("normalize: 未知の種類 %d", int(k))
}

// forbiddenRune は、文書にも診断にも出さない文字か。改行とタブ以外の制御文字 (C0・DEL・C1)、書式制御文字 (Cf。
// 表示の向きを変える双方向制御 (Trojan Source)・ゼロ幅の文字・BOM・タグ文字など)、行・段落の区切り (U+2028・U+2029)、
// 見えない文字 (isDefaultIgnorable)、点字の空白 (U+2800。見た目が空白) を含む。見えないので、レビューで読めない内容を
// 隠せる (2 種類の見えない文字の並びで、任意のデータを書ける)。
func forbiddenRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
		return true
	case r == 0x2028 || r == 0x2029, r == 0x2800:
		return true
	}
	return isDefaultIgnorable(r)
}

// isDefaultIgnorable は、Unicode の Default_Ignorable_Code_Point (DerivedCoreProperties.txt。表示しない、見えない文字) の
// うち、Cf (forbiddenRune が、別に全部禁止する) 以外を禁止するかを返す。DerivedCoreProperties の定義は、
// Other_Default_Ignorable_Code_Point + Cf + Variation_Selector から、White_Space などを除いたもの。表は、Go の unicode
// パッケージ (PropList.txt から生成。unicode.Version は、Go 1.24 で 15.0.0) の Other_Default_Ignorable_Code_Point (Hangul
// filler U+115F・U+1160・U+3164・U+FFA0、CGJ U+034F、Khmer U+17B4・U+17B5、未割当の U+2065・U+FFF0〜FFF8・U+E0000 台) と
// Variation_Selector (Mongolian の U+180B〜180D・180F と、異体字セレクタ) を使い、個別の文字を並べない。
// 除くのは、異体字セレクタ U+FE00〜FE0F (絵文字) と U+E0100〜E01EF (漢字の異体字 IVS。日本語で正当に使う) だけ。
func isDefaultIgnorable(r rune) bool {
	if 0xFE00 <= r && r <= 0xFE0F || 0xE0100 <= r && r <= 0xE01EF {
		return false
	}
	return unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r)
}

// checkRunes は、s が正しい UTF-8 で、forbiddenRune を含まないことを確かめる。
func checkRunes(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("不正な UTF-8 を含む")
	}
	for _, r := range s {
		if forbiddenRune(r) {
			return fmt.Errorf("制御文字か、書式制御文字 (見えない文字・表示の向きを変える文字) U+%04X を含む", r)
		}
	}
	return nil
}

// checkSource は、読んだファイル (.go・.md) の中身が、正しい UTF-8 で、forbiddenRune を含まないことを確かめる。
// 入力を構文解析や変換にかける前の生のバイト列で調べる (doc comment の解析は、行末の空白 (FF・VT・U+0085・
// U+2028 など) を黙って取り除くので、解析後の文字列だけを調べても、制御文字を見逃す)。
// 見つけたら、path:行: の形で error にする。
func checkSource(name string, data []byte) error {
	for i, line := range strings.Split(string(data), "\n") {
		if !utf8.ValidString(line) {
			return fmt.Errorf("%s:%d: 不正な UTF-8 を含む", name, i+1)
		}
		for _, r := range line {
			switch {
			case r == '\r':
				return fmt.Errorf("%s:%d: CR を含む (改行は LF にする。CRLF に変換されないよう、.gitattributes で eol=lf にする)", name, i+1)
			case forbiddenRune(r):
				return fmt.Errorf("%s:%d: 制御文字か、書式制御文字 (見えない文字・表示の向きを変える文字) U+%04X を含む", name, i+1, r)
			}
		}
	}
	return nil
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

// urlChars は、リンク先に許す文字。RFC 3986 の unreserved と reserved から、Markdown のリンクの構文を壊しうる
// 括弧と角括弧を除き、% を足したもの。空白・引用符・山括弧・バッククォート・バックスラッシュも含まない。
func urlChars(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
		strings.ContainsRune("-._~:/?#@!$&'*+,;=%", r)
}

// checkURL は、リンク先が、http(s) の絶対 URL か、相対 (path と # だけ) であることを確かめる。
// javascript:・data:・file: などの scheme と、// で始まる (host を指す) 相対、userinfo (user:pass@) は許さない。
func checkURL(s string) error {
	if s == "" {
		return fmt.Errorf("リンク先が空")
	}
	for _, r := range s {
		if !urlChars(r) {
			return fmt.Errorf("リンク先 %q に、許可しない文字 %q がある", s, r)
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("リンク先 %q を解釈できない: %v", s, err)
	}
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		if u.Host == "" || u.User != nil {
			return fmt.Errorf("リンク先 %q の host が無いか、userinfo がある", s)
		}
	case u.Scheme != "":
		return fmt.Errorf("リンク先 %q の scheme %q は許可しない (許可: http・https・相対)", s, u.Scheme)
	case strings.HasPrefix(s, "//") || u.Host != "":
		return fmt.Errorf("リンク先 %q は、host を指す相対 (//) なので許可しない", s)
	}
	return nil
}

// isCJK は、日本語の文字 (漢字・かな・全角の記号) か。改行を挟んだ日本語どうしは、空白を入れずに詰める。
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) ||
		0x3000 <= r && r <= 0x303F || 0xFF00 <= r && r <= 0xFFEF
}

// joinLines は、s の改行とタブを空白にする。改行の前後が日本語どうしなら、空白を入れずに詰める
// (Markdown は、段落内の改行を空白にするので、日本語が途中で割れて見える)。s の先頭と末尾の改行は、空白にする
// (前後の部品との境目)。
func joinLines(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	if !strings.Contains(s, "\n") {
		return s
	}
	lead, trail := strings.HasPrefix(s, "\n"), strings.HasSuffix(s, "\n")
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	var b strings.Builder
	if lead {
		b.WriteByte(' ')
	}
	for i, ln := range lines {
		if i > 0 {
			ln = strings.TrimLeft(ln, " ")
		}
		if i < len(lines)-1 {
			ln = strings.TrimRight(ln, " ")
		}
		if i > 0 && ln != "" {
			prev, _ := utf8.DecodeLastRuneInString(b.String())
			next, _ := utf8.DecodeRuneInString(ln)
			if !(isCJK(prev) && isCJK(next)) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(ln)
	}
	if trail {
		b.WriteByte(' ')
	}
	return b.String()
}

// markdownSpecials は、行内のどこにあっても、バックスラッシュでエスケープする文字。エスケープしても、
// 描画は同じ (CommonMark は、ASCII の記号をエスケープできる)。& は文字参照 (&lt; など) を、< は HTML と autolink を、
// [ と ] はリンクと画像を、| は表のセルを、~ は取り消し線と fence を、$ は数式を、# は見出しを作る。
const markdownSpecials = "\\`*_[]<>&|~$#"

// escapeMarkdown は、文字列を、Markdown の書式として解釈されない文字にする。
// lineStart なら、先頭 (空白の後) の + と - (リスト)、数字列 + . か ) (番号付きリスト) も、エスケープする。
// 部品の途中の先頭でも、余分にエスケープするだけで、描画は変わらない。
func escapeMarkdown(s string, lineStart bool) string {
	var b strings.Builder
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	b.WriteString(s[:i])
	rest := s[i:]
	if lineStart && rest != "" {
		if rest[0] == '+' || rest[0] == '-' {
			b.WriteByte('\\')
		} else if j := digitRun(rest); j > 0 && j < len(rest) && (rest[j] == '.' || rest[j] == ')') {
			b.WriteString(rest[:j])
			b.WriteByte('\\')
			rest = rest[j:]
		}
	}
	for _, r := range rest {
		if strings.ContainsRune(markdownSpecials, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func digitRun(s string) int {
	i := 0
	for i < len(s) && '0' <= s[i] && s[i] <= '9' {
		i++
	}
	return i
}

// longestBackticks は、s の中の、連続するバッククォートの最大の長さ。
func longestBackticks(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// codeSpan は、s を、行内のコード (バッククォートの囲み) にする。囲みは、内容の連続バッククォート数 + 1。
// 内容の先頭か末尾がバッククォートなら、空白を 1 つずつ足す。改行とタブは空白にする。
func codeSpan(s string) string {
	s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	fence := strings.Repeat("`", longestBackticks(s)+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// codeBlock は、s を、コードブロックにする。fence は、内容の連続バッククォート数 + 1 (3 以上) で、
// 内容の中の行が、ブロックを閉じることはない。lang は info string (固定の文字列だけ)。
func codeBlock(lang, s string) string {
	fence := strings.Repeat("`", max(3, longestBackticks(s)+1))
	return fence + lang + "\n" + strings.TrimRight(s, "\n") + "\n" + fence
}

// escapeDiag は、診断の文字列の、禁止する文字 (forbiddenRune) と改行・タブ・不正な UTF-8 を、\x..・\u....・\U........ に直す。
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
		case forbiddenRune(r) && r > 0xFFFF:
			b.WriteString(fmt.Sprintf(`\U%08x`, r))
		case forbiddenRune(r):
			b.WriteString(fmt.Sprintf(`\u%04x`, r))
		default:
			b.WriteRune(r)
		}
		i += w
	}
	return b.String()
}

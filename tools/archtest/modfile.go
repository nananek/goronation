package archtest

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// modStmt は、go.mod / go.work の 1 つの文。"verb ( ... )" のブロックの中の 1 行も、
// verb を付けた 1 つの文にする。
type modStmt struct {
	Line int
	Verb string
	Args []string
}

// parseModFile は、go.mod / go.work の文を取り出す (golang.org/x/mod は使わない。外部依存を持たない)。
//
// 書式は go と同じにする: 空白区切り、"//" から行末までがコメント、"..." の引用、
// verb ( ... ) のブロック。"//" は単語の途中でもコメントの始まりで、go も use ./a//x を ./a と読む。
// 違う読み方をすると、archtest が検査した path と、go が使う path がずれる。
// 解釈できない書式は、黙って読み飛ばさず error にする (見えないものを、違反なしとして扱わない)。
// go は `...` の引用を受け付けない (unquoted string cannot contain quote) ため、これも error にする。
func parseModFile(data []byte) ([]modStmt, error) {
	var stmts []modStmt
	var block string // 開いているブロックの verb。無ければ空
	var blockLine int
	for i, raw := range strings.Split(string(data), "\n") {
		line := i + 1
		toks, err := modTokens(raw)
		if err != nil {
			return nil, fmt.Errorf("%d 行目: %w", line, err)
		}
		if len(toks) == 0 {
			continue
		}
		hasParen := slices.ContainsFunc(toks, func(s string) bool { return s == "(" || s == ")" })
		switch {
		case block != "" && len(toks) == 1 && toks[0] == ")":
			block = ""
		case hasParen && block == "" && len(toks) == 2 && toks[1] == "(" && toks[0] != "(" && toks[0] != ")":
			block, blockLine = toks[0], line
		case hasParen:
			return nil, fmt.Errorf("%d 行目: 括弧の位置を解釈できない", line)
		case block != "":
			stmts = append(stmts, modStmt{Line: line, Verb: block, Args: toks})
		default:
			stmts = append(stmts, modStmt{Line: line, Verb: toks[0], Args: toks[1:]})
		}
	}
	if block != "" {
		return nil, fmt.Errorf("%d 行目の %s ( が閉じていない", blockLine, block)
	}
	return stmts, nil
}

// modTokens は 1 行を語に分ける。"(" と ")" は、それだけで 1 つの語にする。
// 引用符の語は中身を取り出す。コメントは捨てる。
func modTokens(line string) ([]string, error) {
	var toks []string
	for {
		line = strings.TrimLeft(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "//") {
			return toks, nil
		}
		var tok string
		switch line[0] {
		case '(', ')':
			tok, line = line[:1], line[1:]
		case '`':
			return nil, errors.New("` の引用は、go が受け付けない")
		case '"':
			end := quoteEnd(line)
			if end < 0 {
				return nil, errors.New("引用符が閉じていない")
			}
			s, err := strconv.Unquote(line[:end+1])
			if err != nil {
				return nil, fmt.Errorf("引用符の中身を解釈できない: %w", err)
			}
			if s == "(" || s == ")" {
				return nil, errors.New("括弧を引用符で包んだ語は解釈しない")
			}
			tok, line = s, line[end+1:]
			// 引用符の直後は、語の切れ目でなければならない。
			if line != "" && !strings.ContainsRune(" \t\r()", rune(line[0])) && !strings.HasPrefix(line, "//") {
				return nil, errors.New("引用符の直後に語が続いている")
			}
		default:
			end := len(line)
			if i := strings.IndexAny(line, " \t\r()"); i >= 0 {
				end = i
			}
			if i := strings.Index(line[:end], "//"); i >= 0 {
				end = i
			}
			tok, line = line[:end], line[end:]
		}
		toks = append(toks, tok)
	}
}

// quoteEnd は、line の先頭の引用符 (") に対応する、閉じる引用符の位置を返す。無ければ -1。
func quoteEnd(line string) int {
	for i := 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

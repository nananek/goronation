package archtest

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// dump は文を "<行>:<verb>:<引数を | で連結>" にする。
func dump(stmts []modStmt) []string {
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		out = append(out, fmt.Sprintf("%d:%s:%s", s.Line, s.Verb, strings.Join(s.Args, "|")))
	}
	return out
}

func TestParseModFile(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"1 行の文", "module a/b\n\ngo 1.24.0\n", []string{"1:module:a/b", "3:go:1.24.0"}},
		{"コメントと空行", "// 先頭\n\nuse ./a // 行末\n  // 字下げしたコメント\n", []string{"3:use:./a"}},
		{"ブロック", "use (\n\t./a // c\n\n\t\"./b c\"\n)\nuse ./e\n", []string{
			"2:use:./a", "4:use:./b c", "6:use:./e",
		}},
		{"replace のブロック", "replace (\n\ta v1.0.0 => ./b\n\tc => ../d\n)\n", []string{
			"2:replace:a|v1.0.0|=>|./b", "3:replace:c|=>|../d",
		}},
		// go は、単語の途中の "//" もコメントの始まりとして読む (use ./a//x は ./a)。
		// 違う読み方をすると、archtest が検査した path と、go が使う path がずれる。
		{"単語の途中の //", "use ./a//x\n", []string{"1:use:./a"}},
		{"引用符の中の //", "use \"./a//x\"\n", []string{"1:use:./a//x"}},
		{"引用符の中のエスケープ", "use \"./a\\\"b\"\n", []string{"1:use:./a\"b"}},
		{"CRLF", "use ./a\r\nuse (\r\n\t./b\r\n)\r\n", []string{"1:use:./a", "3:use:./b"}},
		{"括弧は空白なしでも別の語", "use(\n./a\n)\n", []string{"2:use:./a"}},
		{"空", "", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parseModFile([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if got := dump(stmts); !slices.Equal(got, tc.want) {
				t.Errorf("got:\n%s\nwant:\n%s", lines(got), lines(tc.want))
			}
		})
	}
}

// TestParseModFileErrors は、解釈できない書式を、黙って読み飛ばさず error にすることを確認する。
func TestParseModFileErrors(t *testing.T) {
	cases := map[string]string{
		"ブロックが閉じていない":      "use (\n\t./a\n",
		"対応の無い閉じ括弧":        ")\n",
		"開き括弧の後ろに語が続く":     "use ( ./a\n)\n",
		"閉じ括弧の前に語がある":      "use (\n\t./a )\n",
		"1 行の中の括弧":         "replace ( a => b )\n",
		"ブロックの中の入れ子":       "use (\n\tx (\n)\n",
		"引用符が閉じていない":       "use \"./a\n",
		"go が受け付けない生文字列":   "use `./a`\n",
		"引用符の直後に語が続く":      "use \"./a\"b\n",
		"引用符の中身が解釈できない":    "use \"\\q\"\n",
		"括弧を引用符で包んだもの":     "use \"(\"\n",
		"括弧だけの行を verb にする": "( a\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if stmts, err := parseModFile([]byte(in)); err == nil {
				t.Fatalf("error を返すべき: %v", dump(stmts))
			}
		})
	}
}

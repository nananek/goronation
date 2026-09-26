package main

import (
	"strings"
	"testing"
)

// 攻撃者視点レビュー (2d63f6d) の finding 5 (限界): md-place・md-size は、辿らない名前と、.md 以外の拡張子で、迂回できる。
//
// md-place の目的は、「別の場所に移して、上限 (README 2,000 字・ADR 4,000 字) を回避できない」こと (doc.go の規則)。
// しかし、skipDir の名前 (testdata・vendor・. や _ で始まるディレクトリ) の下の .md は、辿らないので検査されない
// (doc.go の「限界」に、この一文はある)。また、isMarkdown は .md と .markdown だけで、.mdx・.txt・.rst などの
// 文書は、置き場も大きさも見ない (こちらは、doc.go に書いていない)。
// このテストは、この限界を固定する (期待は「指摘が出ない」)。直したら赤になるので、期待を反転し、doc.go の「限界」を直す。
func TestMarkdownPlacementLimitsPassThrough(t *testing.T) {
	big := strings.Repeat("あ", 50000) + "\n" // README の上限 2,000 字の 25 倍
	for _, p := range []string{
		"_notes/big.md", ".hidden/big.md", "testdata/big.md", "vendor/big.md", // 辿らない名前
		"docs/big.txt", "docs/big.mdx", "docs/big.rst", "docs/big.adoc", // .md 以外の拡張子
	} {
		t.Run(p, func(t *testing.T) {
			expectRules(t, lintFindings(t, map[string]string{"core/doc.go": goodDoc("core"), p: big})) // 指摘なし (限界が直ると、md-place か md-size が出て赤になる)
		})
	}
}

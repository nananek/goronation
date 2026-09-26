package main

import "testing"

// 攻撃者視点レビュー (2d63f6d) の finding 4 (限界): 手書きの .md の md-link は、scheme を使ったリンクの一部を見ない。
//
// markdownLinks は、[text](url) と [ref]: url だけを解析する。次は、リンクとして扱われず、md-link が出ない
// (実測: marked は <javascript:…> の autolink と、生の HTML の href を、そのまま href に出す。markdown-it は、
// autolink を止めるが、生の HTML は通す。GitHub は、描画のときに sanitize する)。
//   - autolink: <javascript:alert(1)>・<data:text/html;base64,…>
//   - 生の HTML: <a href="javascript:…">
//   - 文字参照: [x](&#106;avascript:alert(1))。先頭が & なので scheme とみなされず、# で切った "&" が、同じ dir に
//     ファイルとして実在すれば (decoy)、相対リンクとして通る。
//
// このテストは、この限界を固定する (期待は「指摘が出ない」)。直したら赤になるので、期待を反転し、doc.go の「限界」を直す。
func TestMarkdownLinkLimitsPassThrough(t *testing.T) {
	cases := []struct{ name, md string }{
		{"autolink javascript", "<javascript:alert(1)>\n"},
		{"autolink data", "<data:text/html;base64,PHNjcmlwdD4=>\n"},
		{"生の HTML", "<a href=\"javascript:alert(1)\">x</a>\n"},
		{"文字参照 + decoy", "[x](&#106;avascript:alert(1))\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				"core/doc.go":        goodDoc("core"),
				"docs/adr/0001-a.md": "# 0001. 最初\n- 状態: 採用\n\n" + tc.md,
				"docs/adr/&":         "decoy (md-link の存在確認だけを通す)\n",
			}
			expectRules(t, lintFindings(t, files)) // 指摘なし (限界が直ると、md-link が出て赤になる)
		})
	}
}

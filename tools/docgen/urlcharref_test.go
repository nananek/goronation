package main

import "testing"

// 攻撃者視点レビュー (2d63f6d) の finding 8 (Nit。今は到達できない): checkURL は、文字参照を復号した後の scheme を見ない。
//
// CommonMark は、リンク先の文字参照 (&#106; や &colon;) を復号する。"&#106;avascript:alert%281%29" は、
// url.Parse では scheme が空 (先頭が & なので scheme ではない。# 以降は fragment) で、checkURL を通るが、
// 描画すると javascript: のリンクになりうる (marked で href="&#106;avascript:…" を確認。GitHub は sanitize する)。
// 今は、go/doc/comment のリンク定義と自動リンクが、scheme を file・ftp・gopher・http・https・mailto・nntp の
// "scheme://" に限るので、doc comment からは到達できない (実測: 3 つの表記で、リンクにならない)。
// パーサの実装が変わると (または kindURL の呼び出しが増えると)、checkURL だけが防御になる。多層にするため、
// 文字参照を含むリンク先は、error にする (& は、クエリの区切りとして正当なので、& + # か & + 英字 + ; の形だけを見る)。
func TestCheckURLRejectsCharacterReferences(t *testing.T) {
	for _, s := range []string{
		"&#106;avascript:alert%281%29",
		"&#x6a;avascript:alert%281%29",
		"javascript&colon;alert%281%29",
		"data&colon;text/html,x",
		"/a?x=1&#106;avascript:x", // 途中にあっても、復号すると意味が変わりうる形は、error
		"/a?x=&frac12;",           // 数字を含む名前の文字参照
		"/a?x=&amp;y",             // 名前の文字参照
		"/a?x&#",                  // 末尾の & と #
	} {
		if err := checkURL(s); err == nil {
			t.Errorf("checkURL(%q) が error にならない (文字参照を含む)", s)
		}
	}
	for _, s := range []string{"https://example.com/a?x=1&y=2", "core.md#anchor", "../docs/adr/0001-a.md",
		"/a?x&y", "/a?x=1&y=2;z=3", "/a#x&amp", "/a?x=1&y2=z"} { // & の後ろが、# でも、英数字 + ; でもない & は、正当
		if err := checkURL(s); err != nil {
			t.Errorf("checkURL(%q): %v (正当なリンクを拒否してはいけない)", s, err)
		}
	}
}

package main

import (
	"strings"
	"testing"
)

// 異体字セレクタ (U+FE00〜FE0F と U+E0100〜E01EF) の置き場所 (攻撃者視点レビュー 5135b8f の finding 1)。
// 256 個 = ちょうど 1 バイトの文字集合で、見えないので、連続して並べると、レビューで読めない任意のデータを運べる
// (レビュー時は、129 個の連続が、doc comment・手書き .md・生成物・-check を通った)。
// 直前が、異体字セレクタ・空白 (改行を含む)・行頭のときは、error にする (連続と、基底の文字の無い単独を禁止)。
// 基底の文字に 1 個だけ付けた形 (漢字 + IVS・絵文字 + VS16) は、日本語と絵文字で正当に使うので、通す。
// 残る限界: 1 文字ごとに 1 個ずつの交互は、正当な使い方と区別できず、通る (1 文字あたり 1 バイトの帯域が残る)。
// TestVariationSelectorAlternationPassesThrough が固定している。

const (
	vs1     = rune(0xFE00)  // 異体字セレクタ 1 (VS1)
	vs16    = rune(0xFE0F)  // 絵文字の異体字セレクタ (VS16)
	ivs     = rune(0xE0100) // 漢字の異体字セレクタ (IVS。VS17)
	ivsLast = rune(0xE01EF) // 異体字セレクタの範囲の最後
)

// vsOf は、バイト b を、異体字セレクタ 1 文字にする (0〜15 は U+FE00〜FE0F、16〜255 は U+E0100〜E01EF)。
func vsOf(b byte) rune {
	if b < 16 {
		return 0xFE00 + rune(b)
	}
	return 0xE0100 + rune(b-16)
}

// vsByte は、r が異体字セレクタなら、vsOf の逆のバイトを返す。実装の isVariationSelector を使わない、テスト側の基準。
func vsByte(r rune) (byte, bool) {
	switch {
	case 0xFE00 <= r && r <= 0xFE0F:
		return byte(r - 0xFE00), true
	case 0xE0100 <= r && r <= 0xE01EF:
		return byte(r-0xE0100) + 16, true
	}
	return 0, false
}

// vsHide は、s の各バイトを、異体字セレクタの列にする。
func vsHide(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteRune(vsOf(s[i]))
	}
	return b.String()
}

// vsReveal は、text の中の異体字セレクタだけを拾って、元のバイト列に戻す。
func vsReveal(text string) string {
	var out []byte
	for _, r := range text {
		if b, ok := vsByte(r); ok {
			out = append(out, b)
		}
	}
	return string(out)
}

// vsCount は、text の中の異体字セレクタの数。
func vsCount(text string) int {
	return len(vsReveal(text))
}

// runes は、rs を並べた文字列 (テストの文字列を、\u のエスケープではなく、数値で組み立てる)。
func runes(rs ...rune) string {
	return string(rs)
}

// vsCase は、置き場所の判定の 1 例。bad なら、keep は、診断 (escapeDiag) に生のまま残る異体字セレクタの数 (基底の文字に
// 付いているもの)。通る例は、診断も変えない。
type vsCase struct {
	name string
	text string
	bad  bool
	keep int
}

func vsCases() []vsCase {
	return []vsCase{
		// 通る: 基底の文字に 1 個だけ付ける。
		{name: "漢字 + IVS 1 個", text: runes('葛', ivs)},
		{name: "漢字 + IVS (範囲の最後)", text: runes('辻', ivsLast)},
		{name: "漢字 + VS1", text: runes('漢', vs1)},
		{name: "絵文字 + VS16", text: runes('❤', vs16)},
		{name: "記号 + VS16 (文の途中)", text: "注意 " + runes('⚠', vs16) + " です"},
		{name: "キーキャップ (数字 + VS16 + U+20E3)", text: runes('1', vs16, 0x20E3)},
		{name: "漢字 + IVS を、間を空けて 2 回", text: runes('葛', ivs) + "と" + runes('辻', ivs)},
		{name: "基底が ASCII でも、1 個は通る", text: runes('a', vs16)},

		// error: 連続。
		{name: "連続 2 個 (VS16 + VS16)", text: runes('❤', vs16, vs16), bad: true, keep: 1},
		{name: "連続 2 個 (IVS + IVS)", text: runes('葛', ivs, ivs), bad: true, keep: 1},
		{name: "連続 2 個 (VS16 + IVS)", text: runes('葛', vs16, ivs), bad: true, keep: 1},
		{name: "連続 3 個 (範囲の両端を含む)", text: runes('葛', vs1, ivsLast, ivs), bad: true, keep: 1},
		{name: "連続が、文の途中に、1 か所だけ", text: "前" + runes('葛', ivs) + "と" + runes('辻', ivs, ivs) + "後", bad: true, keep: 2},

		// error: 基底の文字の無い単独。
		{name: "先頭の単独 (VS16)", text: runes(vs16) + "文", bad: true},
		{name: "先頭の単独 (IVS)", text: runes(ivs) + "文", bad: true},
		{name: "空白の直後", text: "文 " + runes(vs16), bad: true},
		{name: "タブの直後", text: "文\t" + runes(ivs), bad: true},
		{name: "全角空白の直後", text: runes('文', 0x3000, ivs), bad: true},
		{name: "NBSP の直後", text: runes('文', 0xA0, vs16), bad: true},
		{name: "改行の直後 (行頭)", text: "文\n" + runes(vs16), bad: true},
		{name: "改行 + 空白の直後", text: "文\n " + runes(ivs), bad: true},
		{name: "改行を挟んだ、前の行の異体字セレクタの直後", text: runes('葛', ivs) + "\n" + runes(ivs), bad: true, keep: 1},
	}
}

// vsEntries は、text を、異体字セレクタの置き場所を見る入口 (文字を 1 つずつ見る 2 つと、生のバイト列、手書きの .md、
// doc comment の生成) に通し、入口ごとの error (通れば nil) と、doc comment の生成結果を返す。
// 診断 (escapeDiag) は error にせず、エスケープするので、別に見る。
func vsEntries(t *testing.T, text string) (map[string]error, string) {
	t.Helper()
	entries := map[string]error{}
	_, entries["normalize(kindText)"] = normalize(kindText, text)
	entries["checkRunes"] = checkRunes(text)
	entries["checkSource (生のバイト列)"] = checkSource("x/a.md", []byte(text))
	md := "# 0001. 最初\n- 状態: 採用\n\n本文 " + text + " 終。\n"
	_, entries["手書きの .md"] = analyzeFiles(t, map[string]string{"core/doc.go": goodDoc("core"), "docs/adr/0001-a.md": md})
	page := ""
	if !strings.Contains(text, "\n") { // doc comment は、行ごとに書くので、改行を含む例は、上の入口で見る
		var err error
		page, err = corePage(t, docComment("Package core は、テスト。", "", "前 "+text+" 後。"))
		entries["doc comment (.go)"] = err
	}
	return entries, page
}

func TestVariationSelectorPlacement(t *testing.T) {
	for _, tc := range vsCases() {
		t.Run(tc.name, func(t *testing.T) {
			entries, page := vsEntries(t, tc.text)
			for name, err := range entries {
				switch {
				case tc.bad && err == nil:
					t.Errorf("%s: error にならない (%q)", name, tc.text)
				case !tc.bad && err != nil:
					t.Errorf("%s: error になった: %v", name, err)
				}
			}
			if tc.bad {
				for _, name := range []string{"checkRunes", "checkSource (生のバイト列)"} {
					if err := entries[name]; err != nil && !strings.Contains(err.Error(), "異体字セレクタ") {
						t.Errorf("%s: 理由が、異体字セレクタの置き場所になっていない: %v", name, err)
					}
				}
			} else if page != "" && !strings.Contains(page, tc.text) {
				t.Errorf("通る例が、生成物に、そのまま出ない:\n%s", page)
			}

			out, err := normalize(kindDiag, tc.text)
			if err != nil {
				t.Fatalf("normalize(kindDiag) = error %v (診断は、error にしない)", err)
			}
			switch {
			case !tc.bad && out != tc.text:
				t.Errorf("診断が、通る例を変えた: %q → %q", tc.text, out)
			case tc.bad && vsCount(out) != tc.keep:
				t.Errorf("診断に残った異体字セレクタが %d 個, want %d (基底の文字に付いた分だけ): %q", vsCount(out), tc.keep, out)
			}
		})
	}
}

// TestVariationSelectorRunIsError は、レビューが再現した、隠しデータの連続 (129 バイト = 129 個の連続) が、どの入口でも
// 通らないことを確認する。
func TestVariationSelectorRunIsError(t *testing.T) {
	secret := "HIDDEN-MARKER: これは、レビューでは見えない文章の例 (指示ではない)。" + strings.Repeat("隠", 12)
	hidden := vsHide(secret)
	if vsCount(hidden) < 129 {
		t.Fatalf("隠すバイト列が短い (%d 個。レビューが再現したのは、129 個の連続)", vsCount(hidden))
	}

	t.Run("doc comment から生成物へ", func(t *testing.T) {
		page, err := corePage(t, docComment("Package core は、署名"+hidden+"を検証する。", "", "契約。"))
		if err == nil {
			t.Errorf("連続が、生成物に出た: 隠したバイト列 %q", vsReveal(page))
		}
	})

	t.Run("手書きの .md", func(t *testing.T) {
		md := "# 0001. 最初\n- 状態: 採用\n\n本文" + hidden + "。\n"
		if err := checkSource("docs/adr/0001-a.md", []byte(md)); err == nil {
			t.Error("checkSource が、連続を通した")
		}
		if _, err := analyzeFiles(t, map[string]string{"core/doc.go": goodDoc("core"), "docs/adr/0001-a.md": md}); err == nil {
			t.Error("analyze が、連続を通した")
		}
	})

	t.Run("診断", func(t *testing.T) {
		out, err := normalize(kindDiag, "署名"+hidden+"を検証する。")
		if err != nil {
			t.Fatal(err)
		}
		if got := vsCount(out); got != 1 {
			t.Errorf("診断に残った異体字セレクタが %d 個, want 1 (基底の文字に付いた 1 個だけ): %q", got, out)
		}
	})

	t.Run("長さの境界", func(t *testing.T) {
		for _, n := range []int{1, 2, 3, 16, 129, 256} {
			var b strings.Builder
			b.WriteString("漢")
			for i := 0; i < n; i++ {
				b.WriteRune(vsOf(byte(i)))
			}
			err := checkRunes(b.String())
			if (n == 1) != (err == nil) {
				t.Errorf("基底の文字 + 異体字セレクタ %d 個: checkRunes = %v, want error は n > 1 のとき", n, err)
			}
		}
	})

	t.Run("256 個のどれも、基底の文字に 1 個なら通り、単独と空白の直後は error", func(t *testing.T) {
		for i := 0; i < 256; i++ {
			r := vsOf(byte(i))
			if err := checkRunes(runes('葛', r)); err != nil {
				t.Errorf("U+%04X: 基底の文字に 1 個付けたのが error: %v", r, err)
			}
			if err := checkRunes(runes(r)); err == nil {
				t.Errorf("U+%04X: 単独が通った", r)
			}
			if err := checkRunes(runes(' ', r)); err == nil {
				t.Errorf("U+%04X: 空白の直後が通った", r)
			}
		}
	})
}

// TestVariationSelectorAlternationPassesThrough は、残る限界を固定する (期待は「通る」): 1 文字ごとに 1 個ずつ付ける
// 交互の形は、正当な使い方 (IVS・VS16) と区別できず、通る。1 文字あたり 1 バイトの帯域が残る。
// この限界が直ったら (基底の文字の種類を絞るなど)、このテストが赤になる。期待を反転し、doc.go の「限界」も直す。
func TestVariationSelectorAlternationPassesThrough(t *testing.T) {
	// 隠すのは 18 バイト。可視の文字 (23 文字) 1 つに、1 バイト運べる。
	const secret = "HIDDEN-MARKER: 隠"
	const visible = "漢字を検証する説明文の例です、この長さで運べる"
	var b strings.Builder
	var want []byte
	for i, r := range []rune(visible) {
		b.WriteRune(r)
		if i < len(secret) {
			b.WriteRune(vsOf(secret[i]))
			want = append(want, secret[i])
		}
	}
	text := b.String()

	page, err := corePage(t, docComment("Package core は、"+text+"。", "", "契約。"))
	if err != nil {
		t.Fatalf("error になった (限界が直った。期待を反転し、doc.go の「限界」も直す): %v", err)
	}
	if got := vsReveal(page); got != string(want) {
		t.Errorf("交互の形の隠したバイト列が、生成物に残るはず:\n got %q\nwant %q", got, want)
	}
	if len(want) < 10 {
		t.Fatalf("運んだバイト列が短い (%d)", len(want))
	}

	md := "# 0001. 最初\n- 状態: 採用\n\n本文" + text + "。\n"
	if err := checkSource("docs/adr/0001-a.md", []byte(md)); err != nil {
		t.Errorf("手書きの .md が error (限界が直った。期待を反転し、doc.go の「限界」も直す): %v", err)
	}
}

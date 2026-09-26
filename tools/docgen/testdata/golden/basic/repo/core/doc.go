// Package core は、golden テスト用の package。
//
// 契約の段落。日本語どうしの改行は詰め、
// os/exec のような英数字との境目には、空白を入れる。特殊文字 * _ ` < > & | # $ ~ [ ] \ を含む。
//
// # 使い方
//
//	r := core.New("x")
//	fmt.Println(r.Label())
//
// バッククォートを含むコード:
//
//	echo ```` と ``` を含む
//
// # 規則
//
//   - alpha-rule: 箇条書きの 1 項目目。
//     続きの行を含む。
//   - beta-rule・gamma-rule: 2 項目目。
//
// 番号付きのリスト:
//
//  1. 最初
//  2. 次
//
// 項目の間に空行があるリスト:
//
//   - 一つ目
//
//   - 二つ目
//
// # 方針
//
// [Rules] と [Rules.Label]、[New]、[Level] は、この package の doc link。[Unknown] と [strings.Builder] は、
// リンクにならず、コードとして出る。リンクは [example] を見る。https://example.org/auto も自動でリンクになる。
//
// [example]: https://example.com/a?b=c#d
package core

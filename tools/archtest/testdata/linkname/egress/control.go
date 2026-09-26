package egress

// go:linkname a b は、"//" の直後に空白があるので、ディレクティブではない。

/*
//go:linkname c d
ブロックコメントの中は、ディレクティブではない。
*/

// 文字列の中も、ディレクティブではない。
var s = `
//go:linkname e f
`

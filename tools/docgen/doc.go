// Command docgen は、Go の doc comment から Markdown の文書を生成し、文書の形式を検査する。
//
// 標準ライブラリだけで書き、os/exec を使わない。読み書きは、repo の root を開いた os.Root の内側に限る。
package main

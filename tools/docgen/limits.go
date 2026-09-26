package main

import "time"

// limits は、1 回の実行で共有する予算 (入力の上限)。超えたら、黙って続けず error にする。
// 予算は tree が 1 つだけ持ち、読む・書く・ディレクトリを辿る、すべての操作が同じ勘定に足される
// (操作ごと・判定ごとの予算にすると、小さな操作を多数置くだけで、全体の費用が非有界になる)。
type limits struct {
	Timeout       time.Duration // 実行全体の時間
	MaxEntries    int           // 訪問するディレクトリ項目 (名前) の総数。読まずに無視するものも数える
	MaxFiles      int           // 読む・書くファイルの総数
	MaxFileBytes  int64         // 1 ファイルの大きさ
	MaxTotalBytes int64         // 読む・書くバイトの合計
	// MaxGoBytes は、構文解析する .go (package を作るもの。_test.go と、. か _ で始まる名前を除く) の合計バイト。
	// go/parser・go/doc は、入力 1 バイトあたり約 80 バイトのメモリを使う (最悪の形は、二項式 a+a, の羅列。実測)。
	// MaxTotalBytes (64 MiB) いっぱいだと、ピークは約 3.6 GB になるので、別に上限を設ける。
	MaxGoBytes int64
	MaxDepth   int // ディレクトリの深さ (root 直下を 1 とする)
}

// defaultLimits は、実行時の予算。実在する repo は、これに遠く及ばない。
var defaultLimits = limits{
	Timeout:       60 * time.Second,
	MaxEntries:    100_000,
	MaxFiles:      5_000,
	MaxFileBytes:  1 << 20,
	MaxTotalBytes: 64 << 20,
	MaxGoBytes:    12 << 20, // 最悪の形の入力で、ピーク RSS 約 0.9 GB (16 MiB では約 1.2 GB。実測)
	MaxDepth:      64,
}

// 文書の上限 (docs-check が強制する)。値を変えるときは、ADR と doc.go の記述も、一緒に直す。
const (
	readmeMaxLines = 30
	readmeMaxChars = 2000

	adrMaxLines = 60
	adrMaxChars = 4000

	// pkgDocMaxLines は、package doc の行数 (元のファイルのコメントの行数) の上限。一般の package は 30、
	// 検査・強制を担う package (enforcerPackages) は 120 (限界の節を持つため)。行数を超えたら、package を分ける。
	pkgDocMaxLines      = 30
	enforcerDocMaxLines = 120

	// usageMaxCodeLines は、package doc の「使い方」の節に書くコードの、合計の行数の上限。
	usageMaxCodeLines = 5
)

// enforcerPackages は、検査・強制を担う package (repo 相対のディレクトリ)。「限界」の節が必須で、
// package doc の行数の上限が長い。増やすときは、この一覧の変更が、レビューに見える。
var enforcerPackages = []string{"tools/archtest", "tools/docgen"}

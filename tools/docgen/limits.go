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
	MaxDepth      int           // ディレクトリの深さ (root 直下を 1 とする)
}

// defaultLimits は、実行時の予算。実在する repo は、これに遠く及ばない。
var defaultLimits = limits{
	Timeout:       60 * time.Second,
	MaxEntries:    100_000,
	MaxFiles:      5_000,
	MaxFileBytes:  1 << 20,
	MaxTotalBytes: 64 << 20,
	MaxDepth:      64,
}

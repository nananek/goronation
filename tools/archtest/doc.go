// Package archtest は、依存方向と不変条件 I1 を機械的に強制する import 制約テスト。
//
// 標準ライブラリだけで書き、go/parser でソースを構文解析する。os/exec も使わず、
// archtest 自身も規則の対象で、自己免除しない。
//
// # 規則
//
// 表 (rules.go) に置くもの:
//
//   - exec-import: os/exec と、標準ライブラリで os/exec に依存する package (net/http/cgi など) は、
//     sandbox/** と cmd/** だけ。
//   - exec-call: os.StartProcess・syscall.Exec 系・生 syscall (Syscall / RawSyscall など)・実行可能メモリを作る Mmap / Mprotect の
//     参照も、同じ場所だけ。
//   - cgo・plugin: import "C" と "plugin" は、全面禁止。
//   - unsafe-import: unsafe の import は、sandbox/** と cmd/** だけ (実行可能メモリに書いた機械語を、関数として呼べるため)。
//   - dep-core・dep-agent-sandbox・impl-only-from-cmd: 依存方向。
//
// 検査器 (archtest.go) に組み込んだもの。許可される場所は無く、理由は各規則 ID の定数のコメントに書く:
//
//   - modpath: go.mod の module 行が、規約の module path と一致すること。
//   - unscanned-dir-import: 走査しないディレクトリを含む、リポジトリ内の import path。
//   - non-go-source: Go が build に使う、.go 以外のソース (.s・.c・.h など)。
//   - replace: go.mod / go.work の replace。
//   - go-work-use: root の外・走査しないディレクトリ・go.mod の無いディレクトリを指す、go.work の use。
//   - nested-go-work: root 直下以外の go.work。
//   - linkname: //go:linkname。
//   - vendor-mode: vendor/modules.txt。
//
// # 見えないものは禁止する
//
// archtest が見るのは .go の構文だけで、.git・testdata・vendor と、"." や "_" で始まるディレクトリは走査しない。
// go は、これらの外にあるものも build に使う。アセンブリ 1 ファイル、replace 1 行、go.work の use 1 行、
// vendor/modules.txt 1 つで、import も表の関数の参照も無しに、os/exec を使うコードを取り込める。
// そのため、見えないコードを取り込める経路は、個別に検査せず、あるだけで禁止する (fail-closed)。
// 同じ理由で、ディレクトリの symlink・解釈できない go.mod / go.work・構文解析できない .go は、
// 黙って通さず error にする。
//
// # 限界
//
// 静的検査なので、このテストは事故防止であり、悪意ある実装への防壁ではない。強制の主体は設計ルールとレビュー。
//
//   - 外部 module (require) のコードは走査しない。外部依存はいまは無く、go.mod / go.sum の変更はレビューで見る。
//   - ツリーの外は見えない。環境変数 (GOFLAGS・GOWORK など) や、go の引数 (-overlay など) は、
//     Makefile と CI の設定の変更として、レビューで見る。
//   - 表に無い API は検出しない。syscall の生 syscall は、Windows 以外の全 GOOS で、go doc syscall で数えた全部を
//     表に持つ (Go 1.24 時点)。Windows 専用の API (CreateProcess・SyscallN、(*LazyProc).Call など。メソッドは
//     照合できない) は、対応 OS (Linux・macOS) ではないため表に無い。x/sys/unix の生 syscall は
//     Syscall / Syscall6 / RawSyscall / RawSyscall6 だけで、Syscall9 などは表に無い。標準ライブラリで os/exec に
//     依存する package は、TestStdExecDependents が全数を確かめる (Go の更新で増えると、そのテストが赤になる)。
//   - impl-only-from-cmd は、実装 package 自身の側の import も違反にする (docs/adr/0001 の帰結を参照)。
package archtest

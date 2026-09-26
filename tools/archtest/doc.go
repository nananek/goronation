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
//   - gosym-import: debug/gosym の import は、sandbox/** と cmd/** だけ (実行中のバイナリの pclntab から、関数のアドレスを引けるため)。
//   - reflect-unsafe: reflect.NewAt と、メソッド UnsafePointer・UnsafeAddr・SetPointer・MethodByName (前の 3 つを、名前の文字列から
//     動的に呼べる) は、同じ場所だけ (unsafe を import せずに、unsafe.Pointer を得て、任意のアドレスへ書き込める入口)。
//     メソッドは型情報が無いので、名前だけで検出し、同名の無関係なものも検出する。
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
// 黙って通さず error にする。名前が .go・go.mod・go.work で、通常のファイルではないもの (FIFO・デバイス・ソケット。
// symlink の指す先も) も error にする (go は開いて読むが、writer が無いと止まるので、開かず、種別だけで判定する)。
// vendor/modules.txt は、通常のファイルでなくても、あれば違反にする (go は開いて読む)。
// symlink は、link の位置のファイルとして、archtest のプロセスから読んで検査する。連鎖の途中 (途中のディレクトリの
// symlink の先も、1 段ずつ解決して見る) が、/proc・/dev・/sys を通る symlink は、プロセスごとに (cwd・fd・pid で) 別の
// ものに解決され、archtest が読む実体と、go tool・gofmt・compile が読む実体が別になりうる (実測: /proc/self/cwd/x を指す
// core/link.go で、archtest は無害な実体を検査して緑、go は os/exec を含む実体を build した)。名前が .go・go.mod・go.work なら
// error に、vendor か vendor/modules.txt なら違反にする。root の中 (root が /dev/shm の下でもよい) は対象にしない。
// 判定は、結果を使う名前 (と vendor) の symlink だけで行い、1 回の判定で処理する path の要素は 4096 まで。超える連鎖は、
// 時間の上限のために、黙って通さず、同じ扱い (error か違反) にする (fail-closed)。
//
// # 限界
//
// 静的検査なので、このテストは事故防止であり、悪意ある実装への防壁ではない。強制の主体は設計ルールとレビュー。
//
//   - 純 Go の静的検査では、reflect・実行ファイルのシンボル表 (pclntab)・生のシステムコールを組み合わせて、表に無い
//     関数をアドレスで呼べる。これは実測した (この経路の入口は reflect-unsafe・gosym-import で塞いだが、同種の別の
//     経路は、完全には塞げない。動的なメソッド呼び出しは、MethodByName を禁止するが、Method(i) などの添字での呼び出しは
//     検出しない。自前で pclntab を解析する経路も、塞げない)。reflect.Value.Pointer で得た関数のアドレスと、
//     /proc/self/mem への書き込みで、実行中のコードを書き換えることもできる。禁止する語を一切使わないので、検出できない
//     (fixture reflect-pointer で、検出しないことを固定している)。この検査は lint で、I1 の封じ込めは、檻とレビューが担う。
//   - 外部 module (require) のコードは走査しない。外部依存はいまは無く、go.mod / go.sum の変更はレビューで見る。
//   - ツリーの外は見えない。環境変数 (GOFLAGS・GOWORK・GOROOT など。TestStdExecDependents は GOROOT を読む) や、
//     go の引数 (-overlay など) は、Makefile と CI の設定の変更として、レビューで見る。
//   - root の外の絶対 path を指す symlink は、これまでどおり検査する (archtest が読む実体と、go が読む実体が、同じ
//     ファイルであることに依存する)。/proc・/dev・/sys 以外で、プロセス・マウント名前空間・環境ごとに別のものに解決される
//     場所と、検査から go の実行までの間の差し替えは、検出できない。
//   - 表に無い API は検出しない。syscall の生 syscall は、Windows 以外の全 GOOS で、go doc syscall で数えた全部を
//     表に持つ (Go 1.24 時点)。Windows 専用の API (CreateProcess・SyscallN、(*LazyProc).Call など。メソッドは
//     照合できない) は、対応 OS (Linux・macOS) ではないため表に無い。x/sys/unix の生 syscall は
//     Syscall / Syscall6 / RawSyscall / RawSyscall6 だけで、Syscall9 などは表に無い。標準ライブラリで os/exec に
//     依存する package は、TestStdExecDependents が全数を確かめる (Go の更新で増えると、そのテストが赤になる)。
//   - 実行可能メモリを作る・アドレスで関数を呼ぶ経路のうち、塞いでいるのは、unsafe の import、Mmap・Mprotect の直接の
//     参照、reflect の unsafe アクセサ (セレクタとして書かれた NewAt・UnsafePointer・UnsafeAddr・SetPointer と、
//     MethodByName の禁止)、debug/gosym の import だけ。
//     外部 module (x/sys/unix など) の同種の関数を経由する経路は、検出できるか確かめていない。
//   - 手動で go test を回すときは -count=1 を付ける (module の外の変更は、テストの結果のキャッシュに反映されない。make check は付けている)。
//   - impl-only-from-cmd は、実装 package 自身の側の import も違反にする (docs/adr/0001 の帰結を参照)。
package archtest

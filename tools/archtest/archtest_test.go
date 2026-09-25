package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fixture(name string) string { return filepath.Join("testdata", name) }

// keys は違反を "<path>:<line>: <rule>" の並びにする (detail は golden で別に固定する)。
func keys(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, fmt.Sprintf("%s:%d: %s", v.Path, v.Line, v.Rule))
	}
	return out
}

func lines(ss []string) string {
	if len(ss) == 0 {
		return "  (なし)"
	}
	return "  " + strings.Join(ss, "\n  ")
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFixtures は、fixture ごとに、期待する違反の集合と file:line を完全一致で assert する。
// 違反 0 のケース (exec-ok など) は「落ちてはいけないテスト」の対照。
func TestFixtures(t *testing.T) {
	cases := []struct {
		name    string
		scanned int
		want    []string
	}{
		{"exec-ok", 3, nil},
		{"exec-core", 1, []string{
			"core/x/x.go:3: exec-import",
		}},
		{"exec-alias", 3, []string{
			"egress/alias.go:3: exec-import",
			"egress/blank.go:3: exec-import",
			"egress/dot.go:3: exec-import",
		}},
		{"exec-buildtag", 1, []string{
			"egress/darwin.go:5: exec-import",
		}},
		{"exec-testfile", 1, []string{
			"control/control_test.go:4: exec-import",
		}},
		{"exec-prefix", 2, []string{
			"sandboxx/x.go:3: exec-import",
		}},
		// raw.go は、生 syscall (execve を直接呼べる)。syscall と x/sys/unix の Syscall / Syscall6 /
		// RawSyscall / RawSyscall6 を、それぞれ検出する。sandbox/raw_ok.go は許可の対照。
		{"exec-call", 10, []string{
			"egress/dot.go:3: exec-call",
			"egress/os.go:6: exec-call",
			"egress/paren.go:5: exec-call",
			"egress/raw.go:10: exec-call",
			"egress/raw.go:11: exec-call",
			"egress/raw.go:12: exec-call",
			"egress/raw.go:13: exec-call",
			"egress/raw.go:14: exec-call",
			"egress/raw.go:15: exec-call",
			"egress/raw.go:16: exec-call",
			"egress/raw.go:17: exec-call",
			"egress/ref.go:5: exec-call",
			"egress/sys.go:6: exec-call",
			"egress/unix.go:6: exec-call",
		}},
		{"exec-call-shadow", 3, []string{
			"egress/shadow.go:11: exec-call",
		}},
		{"cgo", 2, []string{
			"core/c.go:4: cgo",
			"sandbox/c.go:4: cgo",
		}},
		{"dep-core", 3, []string{
			"core/x/x.go:3: dep-core",
			"core/x/x.go:3: impl-only-from-cmd",
			"core/z/z.go:3: dep-core",
		}},
		{"dep-agent-sandbox", 2, []string{
			"agent/x/x.go:3: dep-agent-sandbox",
			"sandbox/z/z.go:3: dep-agent-sandbox",
		}},
		{"impl-only-from-cmd", 3, []string{
			"control/y.go:3: impl-only-from-cmd",
			"egress/x.go:3: impl-only-from-cmd",
		}},
		// 走査しないディレクトリ (.hidden / _hidden / testdata / vendor) は、明示的に import されると
		// go tool は build する。その import を違反にする。scanned = 1 は、それらを走査していない確認。
		// sub / testdatax / 外部の testdata は、対照 (違反にしない)。
		{"unscanned-import", 1, []string{
			"core/doc.go:5: unscanned-dir-import",
			"core/doc.go:6: unscanned-dir-import",
			"core/doc.go:8: unscanned-dir-import",
			"core/doc.go:10: unscanned-dir-import",
		}},
		{"modpath", 1, []string{
			"cmd/go.mod:1: modpath",
			"core/go.mod:3: modpath",
			"egress/go.mod:1: modpath",
		}},
		// アセンブリ 1 ファイルで、import も表の関数の参照も無しに execve できる。.go しか見ないと、それに気づけない。
		// scanned = 1 は、evil_amd64.s を .go として数えていない確認。testdata の中の .s は走査しない。
		{"non-go-source", 1, []string{
			"core/evil_amd64.s:1: non-go-source",
		}},
		// go.mod / go.work の replace は、走査しないディレクトリや別の module path のコードを取り込める
		// (core/go.mod に replace を 1 行足すだけで、core/_evil2 の os/exec に依存できる)。全面禁止。
		// コメントの中の replace と、module 名に replace を含む require は、対照 (違反にしない)。
		{"replace", 2, []string{
			"core/go.mod:8: replace",
			"go.work:8: replace",
			"sandbox/go.mod:6: replace",
			"sandbox/go.mod:7: replace",
		}},
		// go.work の use は、リポジトリの root の内側で、走査しないディレクトリを含まず、go.mod を持つ
		// ディレクトリだけを許す。use ./core/_evil で、走査しない module を取り込めるため。
		// go.work は入れ子でも、そのファイルのあるディレクトリからの相対で検査する (core/go.work)。
		// 4・5 行目 (./core ./sandbox) と、14 行目 (Clean すると sandbox) は、許可の対照。
		{"go-work-use", 3, []string{
			"core/go.work:4: go-work-use",
			"go.work:6: go-work-use",
			"go.work:7: go-work-use",
			"go.work:8: go-work-use",
			"go.work:9: go-work-use",
			"go.work:10: go-work-use",
			"go.work:11: go-work-use",
			"go.work:15: go-work-use",
			"go.work:16: go-work-use",
		}},
		// //go:linkname は、名前を変えて表の関数 (os.StartProcess など) を参照でき、セレクタの検出をすり抜ける。
		// 位置を問わず (字下げも) 違反にする。control.go は対照 (空白入り・ブロックコメント・文字列の中)。
		{"linkname", 3, []string{
			"egress/indented.go:4: linkname",
			"egress/x.go:8: linkname",
		}},
		// plugin は、事前ビルドした .so を実行時にロードでき、os/exec も表の関数も要らない。
		// 許可される場所は無い (sandbox/** でも使えない)。
		{"plugin", 2, []string{
			"core/x.go:3: plugin",
			"sandbox/y.go:3: plugin",
		}},
		// 標準ライブラリの中で os/exec を使う package (net/http/cgi など) は、import するだけで子プロセスを起動できる
		// (標準ライブラリの中の import は、exec-import の対象外)。os/exec と同じ場所にだけ許す。
		// control.go は対照 (os/exec に依存しない go/build/constraint と net/http)。
		// vendor/modules.txt があると、go は vendor の中の外部 module を build する (ネットワークも go.sum も要らない)。
		// archtest は vendor を走査しないので、vendor/example.com/evil の os/exec を、core が使える。
		// 走査しない vendor (modules.txt の無い、unscanned-import や empty の vendor) は、これまでどおり違反にしない。
		{"vendor-mode", 1, []string{
			"core/vendor/modules.txt:1: vendor-mode",
			"vendor/modules.txt:1: vendor-mode",
		}},
		{"exec-std", 8, []string{
			"core/build.go:3: exec-import",
			"core/cgi.go:3: exec-import",
			"core/fcgi.go:3: exec-import",
			"core/importer.go:3: exec-import",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs, scanned, err := Check(fixture(tc.name), DefaultRules)
			if err != nil {
				t.Fatalf("Check がエラーを返した: %v", err)
			}
			if scanned != tc.scanned {
				t.Errorf("走査したファイル数 = %d, want %d", scanned, tc.scanned)
			}
			if got := keys(vs); !slices.Equal(got, tc.want) {
				t.Errorf("違反が一致しない\ngot:\n%s\nwant:\n%s", lines(got), lines(tc.want))
			}
		})
	}
}

// TestGoldenOutput は、出力文字列の書式を固定する。
func TestGoldenOutput(t *testing.T) {
	const m = "github.com/nananek/goronation"
	cases := []struct {
		name string
		want []string
	}{
		{"exec-core", []string{
			`core/x/x.go:3: exec-import: import "os/exec" は sandbox/**, cmd/** 以外では使えない`,
		}},
		{"cgo", []string{
			`core/c.go:4: cgo: import "C" は全面禁止`,
			`sandbox/c.go:4: cgo: import "C" は全面禁止`,
		}},
		{"dep-core", []string{
			`core/x/x.go:3: dep-core: core/** から import "` + m + `/sandbox/bwrap" は使えない`,
			`core/x/x.go:3: impl-only-from-cmd: import "` + m + `/sandbox/bwrap" は cmd/** 以外では使えない`,
			`core/z/z.go:3: dep-core: core/** から import "` + m + `/control" は使えない`,
		}},
		{"exec-call", []string{
			`egress/dot.go:3: exec-call: import . "syscall" は呼び出しを判定できないため、sandbox/**, cmd/** 以外では使えない`,
			`egress/os.go:6: exec-call: os.StartProcess は sandbox/**, cmd/** 以外では使えない`,
			`egress/paren.go:5: exec-call: os.StartProcess は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:10: exec-call: syscall.Syscall は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:11: exec-call: syscall.Syscall6 は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:12: exec-call: syscall.RawSyscall は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:13: exec-call: syscall.RawSyscall6 は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:14: exec-call: golang.org/x/sys/unix.Syscall は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:15: exec-call: golang.org/x/sys/unix.Syscall6 は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:16: exec-call: golang.org/x/sys/unix.RawSyscall は sandbox/**, cmd/** 以外では使えない`,
			`egress/raw.go:17: exec-call: golang.org/x/sys/unix.RawSyscall6 は sandbox/**, cmd/** 以外では使えない`,
			`egress/ref.go:5: exec-call: syscall.ForkExec は sandbox/**, cmd/** 以外では使えない`,
			`egress/sys.go:6: exec-call: syscall.Exec は sandbox/**, cmd/** 以外では使えない`,
			`egress/unix.go:6: exec-call: golang.org/x/sys/unix.Exec は sandbox/**, cmd/** 以外では使えない`,
		}},
		{"unscanned-import", []string{
			`core/doc.go:5: unscanned-dir-import: import "` + m + `/core/.hidden" は走査しないディレクトリ ".hidden" を含む (明示 import されると go tool は build するが、archtest は走査しない)`,
			`core/doc.go:6: unscanned-dir-import: import "` + m + `/core/_hidden" は走査しないディレクトリ "_hidden" を含む (明示 import されると go tool は build するが、archtest は走査しない)`,
			`core/doc.go:8: unscanned-dir-import: import "` + m + `/core/testdata" は走査しないディレクトリ "testdata" を含む (明示 import されると go tool は build するが、archtest は走査しない)`,
			`core/doc.go:10: unscanned-dir-import: import "` + m + `/core/vendor" は走査しないディレクトリ "vendor" を含む (明示 import されると go tool は build するが、archtest は走査しない)`,
		}},
		{"modpath", []string{
			`cmd/go.mod:1: modpath: module が "example.com/x" だが、"` + m + `/cmd" であるべき`,
			`core/go.mod:3: modpath: module が "` + m + `/wrong" だが、"` + m + `/core" であるべき`,
			`egress/go.mod:1: modpath: module 行が無い`,
		}},
		{"non-go-source", []string{
			`core/evil_amd64.s:1: non-go-source: evil_amd64.s は、Go が build に使う .go 以外のソース (archtest は検査できない) のため、全面禁止`,
		}},
		{"replace", []string{
			`core/go.mod:8: replace: replace は全面禁止 (archtest が走査しないコードを、別の module path で取り込めるため)`,
			`go.work:8: replace: replace は全面禁止 (archtest が走査しないコードを、別の module path で取り込めるため)`,
			`sandbox/go.mod:6: replace: replace は全面禁止 (archtest が走査しないコードを、別の module path で取り込めるため)`,
			`sandbox/go.mod:7: replace: replace は全面禁止 (archtest が走査しないコードを、別の module path で取り込めるため)`,
		}},
		{"go-work-use", []string{
			`core/go.work:4: go-work-use: use "../missing" は、go.mod を持つディレクトリではない`,
			`go.work:6: go-work-use: use "./missing" は、go.mod を持つディレクトリではない`,
			`go.work:7: go-work-use: use "./nomod" は、go.mod を持つディレクトリではない`,
			`go.work:8: go-work-use: use "./core/_evil" は、走査しないディレクトリ "_evil" を含む`,
			`go.work:9: go-work-use: use "../outside" は、リポジトリの root の外を指す`,
			`go.work:10: go-work-use: use "/no/such/abs" は、リポジトリの root の外を指す`,
			`go.work:11: go-work-use: use "." は、go.mod を持つディレクトリではない`,
			`go.work:15: go-work-use: use "./core/../../outside" は、リポジトリの root の外を指す`,
			`go.work:16: go-work-use: use "./vendor/m" は、走査しないディレクトリ "vendor" を含む`,
		}},
		{"linkname", []string{
			`egress/indented.go:4: linkname: //go:linkname は全面禁止 (名前を変えて、表の関数を参照できるため)`,
			`egress/x.go:8: linkname: //go:linkname は全面禁止 (名前を変えて、表の関数を参照できるため)`,
		}},
		{"plugin", []string{
			`core/x.go:3: plugin: import "plugin" は全面禁止`,
			`sandbox/y.go:3: plugin: import "plugin" は全面禁止`,
		}},
		{"vendor-mode", []string{
			`core/vendor/modules.txt:1: vendor-mode: vendor/modules.txt がある (go は vendor から外部 module を build するが、archtest は vendor を走査しない) ため、全面禁止`,
			`vendor/modules.txt:1: vendor-mode: vendor/modules.txt がある (go は vendor から外部 module を build するが、archtest は vendor を走査しない) ため、全面禁止`,
		}},
		{"exec-std", []string{
			`core/build.go:3: exec-import: import "go/build" は sandbox/**, cmd/** 以外では使えない`,
			`core/cgi.go:3: exec-import: import "net/http/cgi" は sandbox/**, cmd/** 以外では使えない`,
			`core/fcgi.go:3: exec-import: import "net/http/fcgi" は sandbox/**, cmd/** 以外では使えない`,
			`core/importer.go:3: exec-import: import "go/importer" は sandbox/**, cmd/** 以外では使えない`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs, _, err := Check(fixture(tc.name), DefaultRules)
			if err != nil {
				t.Fatalf("Check がエラーを返した: %v", err)
			}
			got := make([]string, len(vs))
			for i, v := range vs {
				got[i] = v.String()
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("出力が一致しない\ngot:\n%s\nwant:\n%s", lines(got), lines(tc.want))
			}
		})
	}
}

// TestEmpty は、走査対象が 0 ファイルなら error になる (空振りで緑にしない) ことを確認する。
// fixture の .go はすべて除外ディレクトリ (vendor / _* / .* / testdata) の中にあり、
// 除外が効いていなければ違反が出て error にならない。
func TestEmpty(t *testing.T) {
	vs, scanned, err := Check(fixture("empty"), DefaultRules)
	if err == nil {
		t.Fatalf("error を返すべき: violations=%v scanned=%d", keys(vs), scanned)
	}
	if scanned != 0 {
		t.Errorf("scanned = %d, want 0", scanned)
	}
}

func TestBadRoot(t *testing.T) {
	cases := map[string]string{
		"存在しない":      fixture("no-such-dir"),
		"ディレクトリではない": filepath.Join(fixture("exec-core"), "core", "x", "x.go"),
	}
	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Check(root, DefaultRules); err == nil {
				t.Fatal("error を返すべき")
			}
		})
	}
}

func TestEmptyModuleIsError(t *testing.T) {
	if _, _, err := Check(fixture("exec-ok"), Rules{}); err == nil {
		t.Fatal("Module が空の規則表は error にすべき (import 規則が黙って空振りするため)")
	}
}

// TestParseErrorIsError は、構文解析できない .go を黙って無視しないことを確認する。
func TestParseErrorIsError(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"core/ok.go":  "package core\n",
		"core/bad.go": "package core\n\nimport (\n",
	})
	_, _, err := Check(root, DefaultRules)
	if err == nil {
		t.Fatal("error を返すべき")
	}
	if !strings.Contains(err.Error(), "core/bad.go") {
		t.Errorf("error に対象ファイルが含まれない: %v", err)
	}
}

// TestNonGoSourceExts は、go/build が build に使う .go 以外のソースの拡張子を、全部検出することを確認する。
// 一覧は、go/build の (*Context).matchFile の switch (GOROOT/src/go/build/build.go) と同じで、
// 大文字小文字を区別する (.C や .CPP は Go が build に使わない)。symlink も、名前で検出する。
func TestNonGoSourceExts(t *testing.T) {
	exts := []string{
		".c", ".cc", ".cpp", ".cxx", ".m", ".h", ".hh", ".hpp", ".hxx",
		".f", ".F", ".for", ".f90", ".s", ".S", ".sx", ".swig", ".swigcxx", ".syso",
	}
	ignored := []string{".C", ".CPP", ".txt", ".sh", ".py", ".sy", ".swigc", ".gos", ".md", ""}

	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	files := map[string]string{"root/core/doc.go": "package core\n"}
	var paths []string // Check は path の順に並べる
	for _, e := range exts {
		files["root/core/x"+e] = "\n"
		paths = append(paths, "core/x"+e)
	}
	for _, e := range ignored {
		files["root/core/y"+e] = "\n"
	}
	writeTree(t, tmp, files)
	if err := os.Symlink(filepath.Join(tmp, "root", "core", "doc.go"), filepath.Join(root, "core", "link.s")); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	paths = append(paths, "core/link.s")
	slices.Sort(paths)
	var want []string
	for _, p := range paths {
		want = append(want, p+":1: non-go-source")
	}

	vs, scanned, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != 1 {
		t.Errorf("scanned = %d, want 1 (.go 以外は数えない)", scanned)
	}
	if got := keys(vs); !slices.Equal(got, want) {
		t.Errorf("違反が一致しない\ngot:\n%s\nwant:\n%s", lines(got), lines(want))
	}
}

// TestBadModFileIsError は、解釈できない go.mod / go.work を、黙って通さず error にすることを確認する。
// go.work は、go の directive (go / toolchain / godebug / use / replace) 以外も error にする。
func TestBadModFileIsError(t *testing.T) {
	cases := []struct{ name, rel, content string }{
		{"go.mod の括弧が閉じていない", "core/go.mod", "module github.com/nananek/goronation/core\n\nrequire (\n\ta v1.0.0\n"},
		{"go.work の括弧が閉じていない", "go.work", "go 1.24.0\n\nuse (\n\t./core\n"},
		{"go.work の未知の directive", "go.work", "go 1.24.0\n\nfrobnicate ./core\n"},
		{"go.work の use の引数が 2 つ", "go.work", "go 1.24.0\n\nuse ./core ./sandbox\n"},
		{"go.work の use の引数が無い", "go.work", "go 1.24.0\n\nuse\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{
				"core/doc.go": "package core\n",
				"core/go.mod": "module github.com/nananek/goronation/core\n",
				tc.rel:        tc.content,
			})
			_, _, err := Check(root, DefaultRules)
			if err == nil {
				t.Fatal("error を返すべき")
			}
			if !strings.Contains(err.Error(), tc.rel) {
				t.Errorf("error に対象ファイルが含まれない: %v", err)
			}
		})
	}
}

// TestSymlinkGoWork は、symlink の go.work も、link の位置のファイルとして検査することを確認する
// (go は symlink の go.work をそのまま読む)。
func TestSymlinkGoWork(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	writeTree(t, tmp, map[string]string{
		"root/core/doc.go": "package core\n",
		"root/core/go.mod": "module github.com/nananek/goronation/core\n",
		"outside/go.work":  "go 1.24.0\n\nreplace example.com/a => ./x\n",
	})
	if err := os.Symlink(filepath.Join(tmp, "outside", "go.work"), filepath.Join(root, "go.work")); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	vs, _, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"go.work:3: replace"}; !slices.Equal(keys(vs), want) {
		t.Errorf("violations=%v (want %v)", keys(vs), want)
	}
}

// TestBadModuleLine は、module 行を解釈できない go.mod (引数が 0 個、または 2 個以上) を、
// 黙って通さず modpath の違反にすることを確認する。
func TestBadModuleLine(t *testing.T) {
	cases := map[string]string{
		"引数が無い":   "module\n",
		"引数が 2 つ": "module a b\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{
				"core/doc.go": "package core\n",
				"core/go.mod": content,
			})
			vs, _, err := Check(root, DefaultRules)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"core/go.mod:1: modpath"}; !slices.Equal(keys(vs), want) {
				t.Fatalf("violations=%v (want %v)", keys(vs), want)
			}
			if want := "module 行を解釈できない"; vs[0].Detail != want {
				t.Errorf("detail = %q, want %q", vs[0].Detail, want)
			}
		})
	}
}

// TestVendorSymlink は、vendor が symlink でも、指す先の modules.txt を見る (go は辿る) ことを確認する。
func TestVendorSymlink(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	writeTree(t, tmp, map[string]string{
		"root/core/doc.go":           "package core\n",
		"outside/vendor/modules.txt": "# example.com/evil v1.0.0\n",
	})
	if err := os.Symlink(filepath.Join(tmp, "outside", "vendor"), filepath.Join(root, "vendor")); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	vs, _, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vendor/modules.txt:1: vendor-mode"}; !slices.Equal(keys(vs), want) {
		t.Errorf("violations=%v (want %v)", keys(vs), want)
	}
}

// TestDirSymlink は、走査対象のツリー内のディレクトリの symlink を、辿らずに error にすることを確認する。
// go tool は、./... では symlink のディレクトリを列挙しないが、明示的に import されれば辿って build する。
// 黙って無視すると、許可された場所 (sandbox/**) のディレクトリへの symlink を core/ に置くだけで、
// 規則をすり抜けられる。
func TestDirSymlink(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	writeTree(t, tmp, map[string]string{
		"root/core/ok.go":       "package core\n",
		"outside/runner/run.go": "package runner\n\nimport \"os/exec\"\n\nvar Run = exec.Command\n",
	})
	outside := filepath.Join(tmp, "outside", "runner")
	link := filepath.Join(root, "core", "runner")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	_, _, err := Check(root, DefaultRules)
	if err == nil {
		t.Fatal("ディレクトリの symlink は error にすべき")
	}
	if !strings.Contains(err.Error(), "core/runner") {
		t.Errorf("error に対象の symlink が含まれない: %v", err)
	}

	// 走査しないディレクトリの名前の symlink は、実体のディレクトリと同じく走査しない。
	// その import は、unscanned-dir-import の違反になる。
	// build に使われない、壊れた symlink (.go / go.mod 以外) は無視する。
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{
		"core/use.go": "package core\n\nimport _ \"github.com/nananek/goronation/core/_runner\"\n",
	})
	if err := os.Symlink(outside, filepath.Join(root, "core", "_runner")); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	if err := os.Symlink(filepath.Join(tmp, "no-such"), filepath.Join(root, "core", "dangling")); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	vs, scanned, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"core/use.go:3: unscanned-dir-import"}; !slices.Equal(keys(vs), want) || scanned != 2 {
		t.Errorf("violations=%v scanned=%d (want %v / 2)", keys(vs), scanned, want)
	}
}

// TestSymlink は、symlink の .go は link の位置のファイルとして検査することを確認する。
// go tool は symlink の .go を通常のファイルとして build するため、追わないと、
// 許可された場所のファイルへの symlink で規則をすり抜けられる。
func TestSymlink(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	writeTree(t, tmp, map[string]string{
		"root/core/ok.go":       "package core\n",
		"outside/egress/bad.go": "package egress\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n",
	})
	link := filepath.Join(root, "core", "link.go")
	if err := os.Symlink(filepath.Join(tmp, "outside", "egress", "bad.go"), link); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}

	vs, scanned, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	// core/link.go は、指す先ではなく、core/ の中のファイルとして検査する。
	if want := []string{"core/link.go:3: exec-import"}; !slices.Equal(keys(vs), want) || scanned != 2 {
		t.Errorf("violations=%v scanned=%d (want %v / 2)", keys(vs), scanned, want)
	}

	// 壊れた symlink の .go は、build もできない。見えないものを黙って無視しない。
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "no-such.go"), link); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	if _, _, err := Check(root, DefaultRules); err == nil {
		t.Error("壊れた symlink の .go は error にすべき")
	}
}

// TestSymlinkGoMod は、symlink の go.mod も link の位置のファイルとして検査する
// (modpath) ことを確認する。go tool は symlink の go.mod をそのまま読むため、
// 追わないと、module path のずれを検知できない。
func TestSymlinkGoMod(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	writeTree(t, tmp, map[string]string{
		"root/core/doc.go": "package core\n",
		"outside/go.mod":   "module example.com/wrong\n\ngo 1.24.0\n",
	})
	if err := os.Symlink(filepath.Join(tmp, "outside", "go.mod"), filepath.Join(root, "core", "go.mod")); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	vs, _, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"core/go.mod:1: modpath"}; !slices.Equal(keys(vs), want) {
		t.Errorf("violations=%v (want %v)", keys(vs), want)
	}
}

// TestDepCoreAllTops は、dep-core が対象とする全トップ module を 1 つずつ確認する。
func TestDepCoreAllTops(t *testing.T) {
	for _, top := range []string{"control", "agent", "sandbox", "egress", "vault", "hostfs", "gateway", "cmd"} {
		t.Run(top, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{
				"core/x.go": "package core\n\nimport _ \"github.com/nananek/goronation/" + top + "/pkg\"\n",
			})
			vs, _, err := Check(root, DefaultRules)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, v := range vs {
				if v.Rule == "dep-core" {
					n++
				}
			}
			if n != 1 {
				t.Errorf("dep-core の違反 = %d 件, want 1 (違反: %v)", n, keys(vs))
			}
		})
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, p string
		want       bool
	}{
		{"sandbox/**", "sandbox/bwrap/x.go", true},
		{"sandbox/**", "sandbox/x.go", true},
		{"sandbox/**", "sandbox", true},
		{"sandbox/**", "sandboxx/x.go", false},
		{"sandbox/**", "x/sandbox/y.go", false},
		{"os/exec/**", "os/exec", true},
		{"os/exec/**", "os/execx", false},
		{"os/exec/**", "os/exec/internal/fdtest", true},
		{"C", "C", true},
		{"C", "CC", false},
		{"os/exec", "os/exec/internal", false},
	}
	for _, tc := range cases {
		if got := match(tc.pattern, tc.p); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.p, got, tc.want)
		}
	}
}

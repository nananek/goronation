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
		{"exec-call", 7, []string{
			"egress/dot.go:3: exec-call",
			"egress/os.go:6: exec-call",
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
		{"modpath", 1, []string{
			"cmd/go.mod:1: modpath",
			"core/go.mod:3: modpath",
			"egress/go.mod:1: modpath",
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
			`egress/ref.go:5: exec-call: syscall.ForkExec は sandbox/**, cmd/** 以外では使えない`,
			`egress/sys.go:6: exec-call: syscall.Exec は sandbox/**, cmd/** 以外では使えない`,
			`egress/unix.go:6: exec-call: golang.org/x/sys/unix.Exec は sandbox/**, cmd/** 以外では使えない`,
		}},
		{"modpath", []string{
			`cmd/go.mod:1: modpath: module が "example.com/x" だが、"` + m + `/cmd" であるべき`,
			`core/go.mod:3: modpath: module が "` + m + `/wrong" だが、"` + m + `/core" であるべき`,
			`egress/go.mod:1: modpath: module 行が無い`,
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

// TestSymlink は、go tool と同じく、symlink のディレクトリは辿らず、
// symlink の .go は link の位置のファイルとして検査することを確認する。
// go tool は symlink の .go を通常のファイルとして build するため、追わないと、
// 許可された場所のファイルへの symlink で規則をすり抜けられる。
func TestSymlink(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	writeTree(t, tmp, map[string]string{
		"root/core/ok.go":       "package core\n",
		"outside/egress/bad.go": "package egress\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n",
	})
	if err := os.Symlink(filepath.Join(tmp, "outside", "egress"), filepath.Join(root, "egress")); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}
	link := filepath.Join(root, "core", "link.go")
	if err := os.Symlink(filepath.Join(tmp, "outside", "egress", "bad.go"), link); err != nil {
		t.Skipf("symlink を作れない: %v", err)
	}

	vs, scanned, err := Check(root, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	// egress/ (ディレクトリの symlink) は辿らず、core/link.go は core/ の中のファイルとして検査する。
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

package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for args, want := range map[string]mode{"": modeGenerate, "-check": modeCheck} {
		var a []string
		if args != "" {
			a = strings.Fields(args)
		}
		got, err := parseArgs(a)
		if err != nil || got != want {
			t.Errorf("parseArgs(%q) = %v, %v, want %v", args, got, err, want)
		}
	}
	// パスの引数は無い (出力先は固定。引数で変えられない)。
	for _, args := range [][]string{{"-x"}, {"check"}, {"-check", "-check"}, {"docs/reference"}, {"-check", "../.."}, {"-out=/tmp"}, {""}} {
		if got, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) = %v, nil, want error", args, got)
		}
	}
}

func runIn(t *testing.T, cwd string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb, func() (string, error) { return cwd, nil })
	return code, out.String(), errb.String()
}

func TestRunUsage(t *testing.T) {
	root := fixtureRepo(t, nil)
	for _, args := range [][]string{{"-x"}, {"a", "b"}, {"docs/reference"}} {
		code, out, errs := runIn(t, filepath.Join(root, "tools", "docgen"), args...)
		if code != 2 {
			t.Errorf("%v: code = %d, want 2", args, code)
		}
		if out != "" || !strings.Contains(errs, "使い方") {
			t.Errorf("%v: stdout = %q, stderr = %q", args, out, errs)
		}
	}
}

func TestRunRefusesWrongCwd(t *testing.T) {
	root := fixtureRepo(t, nil)
	for name, cwd := range map[string]string{
		"root":            root,
		"tools":           filepath.Join(root, "tools"),
		"相対 path":         ".",
		"tools/docgen の下": filepath.Join(root, "tools", "docgen", "sub"),
	} {
		code, out, errs := runIn(t, cwd)
		if code != 1 || out != "" || errs == "" {
			t.Errorf("%s: code = %d, stdout = %q, stderr = %q", name, code, out, errs)
		}
	}

	var out, errb bytes.Buffer
	code := run(nil, &out, &errb, func() (string, error) { return "", errors.New("cwd を得られない") })
	if code != 1 || errb.Len() == 0 {
		t.Errorf("getwd の失敗: code = %d, stderr = %q", code, errb.String())
	}
}

func TestRunGenerate(t *testing.T) {
	root := fixtureRepo(t, scaffold(map[string]string{"core/doc.go": goodDoc("core"), "README.md": "# x\n"}))
	cwd := filepath.Join(root, "tools", "docgen")
	code, out, errs := runIn(t, cwd)
	if code != 0 || errs != "" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, out, errs)
	}
	if !strings.Contains(out, "1 個の package") {
		t.Errorf("stdout = %q", out)
	}
	got := readTree(t, filepath.Join(root, "docs"))
	if got["reference/core.md"] == "" || got["reference/README.md"] == "" {
		t.Errorf("生成物が無い: %v", sortedKeys(got))
	}
	// 2 回目は、何も書かない。
	if code, out, _ := runIn(t, cwd); code != 0 || !strings.Contains(out, "書いた 0") {
		t.Errorf("2 回目: code = %d, stdout = %q", code, out)
	}
}

// TestEmitEscapes は、診断の出口 (emit) が、名前や本文の制御文字と改行を、そのまま出さないことを確認する。
func TestEmitEscapes(t *testing.T) {
	var b bytes.Buffer
	emit(&b, "名前 a\x1b[31mb\nERROR: 偽の行\x07\u202e")
	got := b.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Errorf("出力が 1 行ではない: %q", got)
	}
	for _, r := range strings.TrimSuffix(got, "\n") {
		if forbiddenRune(r) || r == '\n' {
			t.Errorf("出力に U+%04X が残った: %q", r, got)
		}
	}
	if !strings.Contains(got, `\x1b[31mb\nERROR`) {
		t.Errorf("エスケープされていない: %q", got)
	}
}

// TestRunEscapesErrorNames は、攻撃者が付けられるファイル名 (制御文字・改行を含む) が、エラー文として出るとき、
// 端末の制御列や偽の行にならないことを確認する。
func TestRunEscapesErrorNames(t *testing.T) {
	for _, name := range []string{"core/a\x1b[2Jb.go", "core/a\nERROR偽.go", "core/a\u202eb.go"} {
		t.Run(name, func(t *testing.T) {
			root := fixtureRepo(t, map[string]string{"core/doc.go": "package core\n", name: "package x\n"})
			code, out, errs := runIn(t, filepath.Join(root, "tools", "docgen"))
			if code != 1 || out != "" {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, out, errs)
			}
			if strings.Count(errs, "\n") != 1 {
				t.Errorf("エラーが 1 行ではない (偽の行を作れる): %q", errs)
			}
			for _, r := range strings.TrimSuffix(errs, "\n") {
				if forbiddenRune(r) || r == '\n' {
					t.Errorf("stderr に U+%04X が残った: %q", r, errs)
				}
			}
		})
	}
}

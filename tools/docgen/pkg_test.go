package main

import "testing"

func TestParseModulePath(t *testing.T) {
	ok := map[string]string{
		"module example.com/m\n\ngo 1.24.0\n":                       "example.com/m",
		"// コメント\nmodule   example.com/m   // 行末のコメント\n":            "example.com/m",
		"go 1.24.0\n\nmodule example.com/m\n":                       "example.com/m",
		"module \"example.com/quoted\"\n":                           "example.com/quoted",
		"module `example.com/raw`\n":                                "example.com/raw",
		"module a/b\nmodule c/d\n":                                  "a/b", // 最初の module 行だけ
		"module example.com/m\r\n":                                  "example.com/m",
		"require x v1\nmodule example.com/after-other-directives\n": "example.com/after-other-directives",
	}
	for in, want := range ok {
		if got, err := parseModulePath([]byte(in)); err != nil || got != want {
			t.Errorf("parseModulePath(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "go 1.24.0\n", "module\n", "module a b\n", "module (\n\texample.com/m\n)\n", "module \"unterminated\n", "module ../evil\n",
		"module example.com/a b\n", "module /abs\n", "module \"\"\n", "module \"a\\x1bb\"\n", "modules example.com/m\n",
	}
	for _, in := range bad {
		if got, err := parseModulePath([]byte(in)); err == nil {
			t.Errorf("parseModulePath(%q) = %q, nil, want error", in, got)
		}
	}
}

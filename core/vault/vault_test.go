package vault

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckFilesOK(t *testing.T) {
	files := []StateFile{{"auth.json", []byte("x")}, {"mcp-auth.json", nil}, {".credentials.json", []byte("y")}, {"a/b.json", []byte("z")}}
	if err := CheckFiles(files); err != nil {
		t.Fatalf("CheckFiles = %v", err)
	}
	if err := CheckFiles(nil); err != nil {
		t.Fatalf("CheckFiles(nil) = %v", err)
	}
}

func TestCheckFilesLimits(t *testing.T) {
	big := make([]byte, MaxStateFileSize)
	over := make([]byte, MaxStateFileSize+1)
	many := make([]StateFile, MaxStateFiles+1)
	for i := range many {
		many[i] = StateFile{Name: string(rune('a' + i))}
	}
	nine := many[:MaxStateFiles]
	cases := map[string][]StateFile{
		"9 files":      many,
		"file too big": {{"a", over}},
		"total":        {{"a", big}, {"b", big}, {"c", big}, {"d", big}, {"e", []byte{0}}},
		"dup":          {{"a", nil}, {"a", nil}},
		"empty name":   {{"", nil}},
		"long name":    {{strings.Repeat("a", MaxStateNameLen+1), nil}},
		"abs":          {{"/etc/passwd", nil}},
		"dotdot":       {{"../x", nil}},
		"dotdot mid":   {{"a/../../x", nil}},
		"unclean":      {{"a/./b", nil}},
		"trailing":     {{"a/", nil}},
		"double slash": {{"a//b", nil}},
		"nul":          {{"a\x00b", nil}},
		"newline":      {{"a\nb", nil}},
		"del":          {{"a\x7fb", nil}},
		"c1":           {{"a\u0085b", nil}},
		"bad utf8":     {{"a\xffb", nil}},
		"dot":          {{".", nil}},
	}
	if err := CheckFiles(nine); err != nil {
		t.Fatalf("上限ちょうど (8 個) は通る: %v", err)
	}
	if err := CheckFiles([]StateFile{{"a", big}, {"b", big}, {"c", big}, {"d", big}}); err != nil {
		t.Fatalf("合計が上限ちょうどは通る: %v", err)
	}
	for name, files := range cases {
		err := CheckFiles(files)
		if !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: err = %v, want ErrInvalidState", name, err)
		}
	}
}

func TestCheckFilesErrorHasNoContent(t *testing.T) {
	err := CheckFiles([]StateFile{{"secret-name\x00", []byte("secret-data")}})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error の文言に内容・名前を含めない: %v", err)
	}
}

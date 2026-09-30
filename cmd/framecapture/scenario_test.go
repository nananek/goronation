//go:build linux

package main

import (
	"github.com/nananek/goronation/cmd/internal/framenorm"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeScenario(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadScenarioDecodesFakeSteps(t *testing.T) {
	p := writeScenario(t, `{
		"turns": ["a", "b"],
		"files": {"x/y.txt": "hi"},
		"expect_exit": {"claude": 1},
		"claude": [{"text": "t", "toolUse": {"id": "toolu_1", "name": "Read", "input": {"file_path": "/work/x"}}}, {"text": "done"}],
		"opencode": [{"content": "c", "toolCalls": [{"id": "call_1", "name": "read", "arguments": {"filePath": "/work/x"}}]}, {"error": {"status": 400, "type": "invalid_request_error", "message": "m"}}]
	}`)
	s, err := loadScenario(p)
	if err != nil {
		t.Fatalf("loadScenario: %v", err)
	}
	if len(s.Turns) != 2 || s.ExpectExit.Claude != 1 || s.ExpectExit.Opencode != 0 {
		t.Fatalf("s = %+v", s)
	}
	if s.Claude[0].ToolUse == nil || s.Claude[0].ToolUse.Name != "Read" || string(s.Claude[0].ToolUse.Input) != `{"file_path": "/work/x"}` {
		t.Fatalf("claude[0] = %+v", s.Claude[0])
	}
	if len(s.Opencode[0].ToolCalls) != 1 || s.Opencode[0].ToolCalls[0].Name != "read" {
		t.Fatalf("opencode[0] = %+v", s.Opencode[0])
	}
	if e := s.Opencode[1].Error; e == nil || e.Status != 400 || e.Type != "invalid_request_error" {
		t.Fatalf("opencode[1] = %+v", s.Opencode[1])
	}
}

func TestLoadScenarioRejects(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"未知のキー (綴りの誤り)":                 {`{"turns":["a"],"claude":[{"tool_use":{}}]}`, "unknown field"},
		"turns が空":                      {`{"turns":[]}`, "turns が空"},
		"絶対 path":                       {`{"turns":["a"],"files":{"/etc/x":"y"}}`, "相対 path"},
		"親を辿る path":                     {`{"turns":["a"],"files":{"../x":"y"}}`, "相対 path"},
		"未知の outcome":                   {`{"turns":["a"],"claude_only":true,"claude_permissions":[{"outcome":"Deny"}]}`, "outcome"},
		"権限の場面が claude_only でない":        {`{"turns":["a"],"claude_permissions":[{"outcome":"allow"}]}`, "claude_only"},
		"claude_only が opencode の応答を持つ": {`{"turns":["a"],"claude_only":true,"opencode":[{"content":"x"}]}`, "opencode"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadScenario(writeScenario(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q を含む", err, tc.want)
			}
		})
	}
}

func TestScenarioWriteFiles(t *testing.T) {
	s := &scenario{Files: map[string]string{"a.txt": "A", "d/b.txt": "B"}}
	work := t.TempDir()
	if err := s.writeFiles(work); err != nil {
		t.Fatalf("writeFiles: %v", err)
	}
	for name, want := range s.Files {
		got, err := os.ReadFile(filepath.Join(work, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v (want %q)", name, got, err, want)
		}
	}
}

// コミット済みの場面はすべて読め、両エージェントの応答を持つ (fixtures を採り直す前に、typo に気づける)。
func TestCommittedScenariosLoad(t *testing.T) {
	files, err := filepath.Glob("../../spec/testdata/golden/scenarios/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("場面が見つからない: %v %v", files, err)
	}
	for _, f := range files {
		s, err := loadScenario(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if s.Description == "" || len(s.Claude) == 0 || (!s.ClaudeOnly && len(s.Opencode) == 0) {
			t.Errorf("%s: description・claude・opencode のどれかが空", f)
		}
		// 宣言と実態の食い違いの回帰: claude_only の場面に opencode の fixture が残っていない
		// (opencode が権限承認を観察していないのに、観察したように見える fixture を作らない)。
		if s.ClaudeOnly {
			name := strings.TrimSuffix(filepath.Base(f), ".json")
			if _, err := os.Stat("../../spec/testdata/golden/opencode/" + name + ".ndjson"); err == nil {
				t.Errorf("%s: claude_only の場面なのに opencode の fixture がある", f)
			}
		}
	}
}

// コミット済みの fixtures は、正規化済みである (正規化しても変わらず、実行ごとの値が残っていない)。
func TestCommittedFixturesAreNormalized(t *testing.T) {
	files, _ := filepath.Glob("../../spec/testdata/golden/*/*.*json*")
	var n int
	for _, f := range files {
		if strings.Contains(f, "/scenarios/") {
			continue
		}
		n++
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		again, err := framenorm.NormalizeFrames(raw)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if string(again) != string(raw) {
			t.Errorf("%s: 正規化済みでない (framenorm.NormalizeFrames をかけると変わる)", f)
		}
	}
	if n == 0 {
		t.Fatal("fixtures が見つからない")
	}
}

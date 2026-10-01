package opencode

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// golden の全 permission.asked が、Validate を通る要求になり、spec/v0/conformance_test.go の参照実装と同じ要約・種類・詳細になる。
func TestPermissionsMapToSummaryAndDetails(t *testing.T) {
	want := map[string]struct {
		summary, kind string
		details       []v0.Detail
	}{
		"shell":              {"shell: echo goro-hi", v0.KindExecute, []v0.Detail{{Label: "command", Text: "echo goro-hi", Kind: v0.DetailCommand}}},
		"edit":               {"edit: notes.txt", v0.KindEdit, nil}, // details[0] は path。patch は後ろに続く
		"read":               {"read: notes.txt", v0.KindRead, []v0.Detail{{Label: "path", Text: "notes.txt", Kind: v0.DetailPath}}},
		"external_directory": {"external_directory: /etc/*", v0.KindRead, []v0.Detail{{Label: "path", Text: "/etc/*", Kind: v0.DetailPath}}},
		"glob":               {"glob: *.txt", v0.KindSearch, []v0.Detail{{Label: "pattern", Text: "*.txt", Kind: v0.DetailText}, {Label: "root", Text: ".", Kind: v0.DetailText}}},
		"grep":               {"grep: goodbye", v0.KindSearch, []v0.Detail{{Label: "pattern", Text: "goodbye", Kind: v0.DetailText}, {Label: "root", Text: ".", Kind: v0.DetailText}}},
		"webfetch": {"webfetch: http://fake-provider.test/page", v0.KindFetch,
			[]v0.Detail{{Label: "url", Text: "http://fake-provider.test/page", Kind: v0.DetailURL}, {Label: "format", Text: "text", Kind: v0.DetailText}}},
		"skill":     {"skill: opencode", v0.KindOther, []v0.Detail{{Label: "target", Text: "opencode", Kind: v0.DetailText}}},
		"subagent":  {"subagent: general", v0.KindOther, []v0.Detail{{Label: "target", Text: "general", Kind: v0.DetailText}}},
		"websearch": {"websearch: goronation", v0.KindSearch, []v0.Detail{{Label: "query", Text: "goronation", Kind: v0.DetailText}}},
		"question":  {"question: *", v0.KindOther, []v0.Detail{{Label: "target", Text: "*", Kind: v0.DetailText}}},
	}
	seen := map[string]bool{}
	hashes := map[string]string{}
	for _, scene := range scenes(t) {
		for _, e := range decodeAll(t, scene) {
			if e.Type != v0.TypePermissionRequested {
				continue
			}
			var p v0.PermissionRequested
			if err := json.Unmarshal(e.Data, &p); err != nil {
				t.Fatal(err)
			}
			if err := p.Validate(); err != nil || p.DetailsTruncated || p.Title != "" {
				t.Fatalf("%s/%s: %v %+v", scene, p.ToolName, err, p)
			}
			w, ok := want[p.ToolName]
			if !ok || seen[p.ToolName] {
				continue
			}
			seen[p.ToolName] = true
			if p.Summary != w.summary || p.Kind != w.kind {
				t.Errorf("%s: summary=%q kind=%q, want %q %q", p.ToolName, p.Summary, p.Kind, w.summary, w.kind)
			}
			if w.details != nil && !slices.Equal(p.Details, w.details) {
				t.Errorf("%s: details = %+v, want %+v", p.ToolName, p.Details, w.details)
			}
			if p.ToolName == "edit" && (p.Details[0].Kind != v0.DetailPath || len(p.Details) < 2 || !strings.HasPrefix(p.Details[1].Label, "patch: ") ||
				!strings.Contains(p.Details[1].Text, "+hello")) {
				t.Errorf("edit の詳細に、path と patch が無い: %+v", p.Details)
			}
			// input は、{action, resources, metadata} だけ (id・sessionID・source・save を含めない)。
			var in map[string]json.RawMessage
			if err := json.Unmarshal(p.Input, &in); err != nil {
				t.Fatal(err)
			}
			for k := range in {
				if k != "action" && k != "resources" && k != "metadata" {
					t.Errorf("%s: input に %q がある", p.ToolName, k)
				}
			}
			if h := p.Hash(); h == "" || hashes[h] != "" {
				t.Errorf("%s: hash が空か、別の要求と同じ", p.ToolName)
			} else {
				hashes[h] = p.ToolName
			}
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("action %q の permission.asked が、golden に無い", k)
		}
	}
}

// shell は、部分コマンドが複数のとき、1 つずつ見せる (1 つでも見えないまま、許可させない。ADR 0021 決定 4)。
func TestShellShowsEveryPartialCommand(t *testing.T) {
	s := started(t)
	envs := feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", `["git status","rm -rf /tmp/x"]`, ""))
	mustTypes(t, envs, v0.TypePermissionRequested)
	var p v0.PermissionRequested
	_ = json.Unmarshal(envs[0].Data, &p)
	if p.Summary != "shell: git status ; rm -rf /tmp/x" || len(p.Details) != 2 || p.Details[1].Text != "rm -rf /tmp/x" || p.DetailsTruncated {
		t.Fatalf("%+v", p)
	}
}

// metadata は、書き換えずに input に載る (数は元の文字列のまま)。
func TestMetadataIsKeptVerbatim(t *testing.T) {
	s := started(t)
	envs := feed(t, s, "permission.asked", asked("per_1", "ses_root", "webfetch", `["http://x"]`, `"metadata":{"url":"http://x","big":12345678901234567890,"nested":{"a":[1,2]}}`))
	var p v0.PermissionRequested
	_ = json.Unmarshal(envs[0].Data, &p)
	if !strings.Contains(string(p.Input), `"big":12345678901234567890`) || !strings.Contains(string(p.Input), `"nested":{"a":[1,2]}`) {
		t.Fatalf("input = %s", p.Input)
	}
}

// 詳細が全体を持てないとき (項目が 16 を超える・1 項目が 4,000 字を超える)、切って DetailsTruncated を立てる。UI は承認させず、
// アダプタも allow を断る。拒否はできる (人間が、見えない要求を、却下できる)。
func TestTruncatedDetailsCannotBeAllowedButCanBeRejected(t *testing.T) {
	for name, resources := range map[string]string{
		"17 個の resources": mustJSON(t, repeat("cmd", 17)),
		"4,001 字":         mustJSON(t, []string{strings.Repeat("あ", v0.MaxDetailTextLen+1)}),
	} {
		t.Run(name, func(t *testing.T) {
			s := started(t)
			envs := feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", resources, ""))
			mustTypes(t, envs, v0.TypePermissionRequested)
			var p v0.PermissionRequested
			_ = json.Unmarshal(envs[0].Data, &p)
			if !p.DetailsTruncated || len(p.Details) > v0.MaxDetails || p.Validate() != nil {
				t.Fatalf("truncated=%v details=%d err=%v", p.DetailsTruncated, len(p.Details), p.Validate())
			}
			if line, syn, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err == nil || line != nil || syn != nil {
				t.Fatalf("切れた要求が、許可できた: %s", line)
			}
			if _, _, err := cmdPermission(t, s, "per_1", v0.RejectOnce); err != nil { // allow を断っても、要求は未決のまま残る
				t.Fatalf("拒否できない: %v", err)
			}
		})
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", s, i)
	}
	return out
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 要約は 1 行・200 字以内 (改行は空白)。詳細は、全体を持つ。
func TestSummaryIsOneLine(t *testing.T) {
	s := started(t)
	cmd := "echo a\necho b # " + strings.Repeat("x", 500)
	envs := feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", mustJSON(t, []string{cmd}), ""))
	var p v0.PermissionRequested
	_ = json.Unmarshal(envs[0].Data, &p)
	if strings.ContainsAny(p.Summary, "\r\n") || len([]rune(p.Summary)) > v0.MaxSummaryLen || p.Details[0].Text != cmd || p.DetailsTruncated {
		t.Fatalf("summary=%q", p.Summary)
	}
}

// 形が不正な要求は、承認できない側 (agent.frame + error。要求を保持しない)。
func TestMalformedPermissionIsNotApprovable(t *testing.T) {
	for name, d := range map[string]string{
		"id が空":            `{"id":"","sessionID":"ses_root","action":"shell","resources":["x"]}`,
		"id に /":           `{"id":"per/../x","sessionID":"ses_root","action":"shell","resources":["x"]}`,
		"id が数":            `{"id":7,"sessionID":"ses_root","action":"shell","resources":["x"]}`,
		"action が空":        `{"id":"per_1","sessionID":"ses_root","action":"","resources":["x"]}`,
		"action が長い":       fmt.Sprintf(`{"id":"per_1","sessionID":"ses_root","action":%q,"resources":["x"]}`, strings.Repeat("a", 129)),
		"resources が文字列":   `{"id":"per_1","sessionID":"ses_root","action":"shell","resources":"x"}`,
		"resources に数":     `{"id":"per_1","sessionID":"ses_root","action":"shell","resources":["x",1]}`,
		"resources が null": `{"id":"per_1","sessionID":"ses_root","action":"shell","resources":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := started(t)
			envs := feed(t, s, "permission.asked", d)
			if len(envs) == 0 || envs[0].Type != v0.TypeAgentFrame {
				t.Fatalf("%v", types(envs))
			}
			for _, e := range envs {
				if e.Type == v0.TypePermissionRequested {
					t.Fatal("承認できる要求になった")
				}
			}
			if len(s.pending) != 0 {
				t.Fatal("要求を保持した")
			}
		})
	}
}

// resources が無い (空配列) 要求は、そのまま見せる (対象が空であることも、承認する内容)。
func TestEmptyResourcesAreShown(t *testing.T) {
	s := started(t)
	envs := feed(t, s, "permission.asked", asked("per_1", "ses_root", "skill", `[]`, ""))
	mustTypes(t, envs, v0.TypePermissionRequested)
}

// 子 session の要求は、Origin つき。応答は子の session ID で返る。
func TestChildPermissionHasOriginAndRepliesToTheChild(t *testing.T) {
	s := started(t)
	child_(t, s, "ses_child", "ses_root")
	feed(t, s, "session.tool.progress", `{"sessionID":"ses_root","id":"call_sub","metadata":{"sessionID":"ses_child"}}`)
	envs := feed(t, s, "permission.asked", asked("per_c", "ses_child", "shell", `["ls"]`, ""))
	if len(envs) != 1 || envs[0].Origin == nil || *envs[0].Origin != (v0.Origin{ID: "ses_child", Parent: "call_sub"}) {
		t.Fatalf("%+v", envs)
	}
	line, syn, err := cmdPermission(t, s, "per_c", v0.AllowOnce)
	if err != nil || string(line) != `{"method":"POST","path":"/api/session/ses_child/permission/per_c/reply","body":{"decision":"once"}}`+"\n" {
		t.Fatalf("%s %v", line, err)
	}
	if syn[0].Origin == nil || syn[0].Origin.ID != "ses_child" {
		t.Fatalf("合成の決着に origin が無い: %+v", syn[0])
	}
}

package claude

import (
	"encoding/json"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// このファイルの test は、Envelope.Public() が守らない Data の側の漏れを、アダプタの側で塞ぐ (Public は Raw だけを落とす)。
// golden fixtures の各フレームの、文字列の値のうち、Data に載せると決めたもの (allowed) 以外は、
// Data のどこにも現れてはならない。frame にしか無い値 (socket の path・memory の path・skill 名・uuid など) が、
// data に紛れ込む変更 (frame の部分木を、そのまま写すなど) で、赤になる。

// leaf は、frame の文字列の値と、その JSON 上の path (配列の添字は * にする)。
type leaf struct{ path, value string }

func leaves(v any, path string, out *[]leaf) {
	switch x := v.(type) {
	case string:
		*out = append(*out, leaf{path, x})
	case map[string]any:
		for k, c := range x {
			leaves(c, path+"."+k, out)
		}
	case []any:
		for _, c := range x {
			leaves(c, path+".*", out)
		}
	}
}

// allowedPaths は、type ごとに、data に載せてよい frame の値の path (接頭辞)。
// 書き換えずに載せると約束した値 (tool の input・tool_result の content・テキスト) と、封筒の語彙が必要とする識別子だけ。
var allowedPaths = map[string][]string{
	"system/permission_denied": {".tool_name", ".tool_use_id"},
	"system/init":              {".session_id", ".cwd", ".model", ".tools", ".permissionMode"},
	"assistant":                {".message.id", ".message.content.*.text", ".message.content.*.id", ".message.content.*.name", ".message.content.*.input"},
	"user":                     {".message.content.*.tool_use_id", ".message.content.*.content"},
	"result":                   {".stop_reason", ".result", ".terminal_reason"},
	// 対話の権限要求。permission_suggestions・display_name・agent_id などは、data に出さない。
	"control_request":        {".request_id", ".request.tool_use_id", ".request.tool_name", ".request.input", ".request.description"},
	"control_cancel_request": {".request_id"},
	"control_response":       {}, // initialize の応答 (account・memory の path などを含む)。data は空
}

var conformanceFixtures = []string{"simple-text", "tool-call", "multi-turn", "permission-request", "error-response",
	"permission-interactive-allow", "permission-interactive-deny", "permission-interactive-interrupt"}

func allowed(kind, path string) bool {
	for _, p := range allowedPaths[kind] {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

func kindOf(f map[string]any) string {
	if f["type"] == "system" {
		return "system/" + f["subtype"].(string)
	}
	return f["type"].(string)
}

func TestDataDoesNotCarryUnintendedFrameValues(t *testing.T) {
	for _, name := range conformanceFixtures {
		s := Adapter{}.NewStream()
		for i, line := range readLines(t, name) {
			var f map[string]any
			if err := json.Unmarshal(line, &f); err != nil {
				t.Fatal(err)
			}
			kind := kindOf(f)
			if _, ok := allowedPaths[kind]; !ok {
				t.Fatalf("%s 行 %d: %s の allowedPaths が無い。fixture が増えたら、表に足す", name, i+1, kind)
			}
			es, err := s.DecodeFrame(line)
			if err != nil {
				t.Fatal(err)
			}
			var ls []leaf
			leaves(f, "", &ls)
			var ok, bad []leaf
			for _, l := range ls {
				if allowed(kind, l.path) {
					ok = append(ok, l)
				} else if len(l.value) >= 4 && l.value != Name { // "text" のような短い値と、アダプタ自身が書く Name は、偶然の一致になる
					bad = append(bad, l)
				}
			}
			for _, e := range es {
				var d any
				if err := json.Unmarshal(e.Data, &d); err != nil {
					t.Fatalf("%s の data が JSON でない: %s", e.Type, e.Data)
				}
				var dl []leaf
				leaves(d, "", &dl)
				for _, l := range bad {
					for _, x := range dl {
						// 許した値の一部に、たまたま含まれる値は漏れではない (例: cwd の /work が、input の /work/hello.txt に含まれる)。
						if strings.Contains(x.value, l.value) && !containedInAllowed(x.value, l.value, ok) {
							t.Errorf("%s 行 %d (%s): frame の %s = %q が、%s の data に出た: %s", name, i+1, kind, l.path, l.value, e.Type, e.Data)
						}
					}
				}
			}
		}
	}
}

// containedInAllowed は、data の値 x が l を含む理由が、許した値 (ok) が l を含むから、だけかを返す。
func containedInAllowed(x, l string, ok []leaf) bool {
	for _, a := range ok {
		if strings.Contains(a.value, l) && strings.Contains(x, a.value) {
			return true
		}
	}
	return false
}

// TestDataKeysAreFixed は、data のキーが、語彙 (spec/v0 の doc) の名前だけであることを固定する。値の漏れを、キーの側から見る。
func TestDataKeysAreFixed(t *testing.T) {
	want := map[string][]string{
		v0.TypeSessionStarted:      {"agent", "agent_session", "cwd", "model", "permission_mode", "tools"},
		v0.TypeMessageText:         {"message_id", "text"},
		v0.TypeToolCall:            {"call_id", "input", "kind", "name", "status"},
		v0.TypeToolUpdate:          {"call_id", "output", "status"},
		v0.TypePermissionRequested: {"call_id", "input", "kind", "request_id", "title", "tool_name"},
		v0.TypePermissionResolved:  {"by", "call_id", "outcome", "tool_name"}, // by=policy。by=human・agent は下で
		v0.TypeUsage: {"cache_creation_input_tokens", "cache_read_input_tokens", "context_window", "cost_usd", "input_tokens",
			"max_output_tokens", "output_tokens", "scope"},
		v0.TypeError:         {"message", "retryable", "status"},
		v0.TypeTurnCompleted: {"is_error", "stop_reason"},
		v0.TypeAgentFrame:    nil,
	}
	seen := map[string]bool{}
	for _, name := range conformanceFixtures {
		for _, e := range decodeAll(t, name) {
			seen[e.Type] = true
			var keys []string
			for k := range data(t, e) {
				keys = append(keys, k)
			}
			w := want[e.Type]
			if e.Type == v0.TypePermissionResolved && data(t, e)["by"] != "policy" { // 対話の決着は、tool ではなく要求の ID を指す
				w = []string{"by", "outcome", "request_id"}
			}
			if e.Type == v0.TypeToolUpdate && data(t, e)["status"] == v0.ToolFailed { // 失敗は、output の代わりに error
				w = []string{"call_id", "error", "status"}
			}
			if !sameSet(keys, w) {
				t.Errorf("%s の data のキー = %v, want %v", e.Type, keys, w)
			}
		}
	}
	for typ := range want {
		if !seen[typ] {
			t.Errorf("%s が、fixture から出なかった", typ)
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

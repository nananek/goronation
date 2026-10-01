package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// このファイルの test は、Envelope.Public() が守らない Data の側の漏れを、アダプタの側で塞ぐ (Public は Raw だけを落とす)。opencode の
// イベントは、location.directory・projectID・slug・sessionID・messageID・provider の URL・response.body を含む。golden の各イベントの
// 文字列の値のうち、Data に載せると決めたもの (allowedPaths) 以外は、Data のどこにも現れてはならない。

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

// allowedPaths は、SSE の type ごとに、data に載せてよい frame の値の path (接頭辞)。書き換えずに載せると約束した値 (tool の input・
// output・テキスト・form・permission の対象) と、封筒の語彙が必要とする識別子だけ。sessionID・messageID・projectID・slug は、
// 載せない (session は Origin・session.started の agent_session だけ)。全ての type に、エントリが要る (空でもよい)。
var allowedPaths = map[string][]string{
	"session.created":               {".data.sessionID", ".data.location.directory", ".data.version"},
	"session.text.ended":            {".data.text", ".data.assistantMessageID"},
	"session.tool.called":           {".data.id", ".data.input"},
	"session.tool.progress":         {".data.id"},
	"session.tool.success":          {".data.id", ".data.content.*.text"},
	"session.tool.failed":           {".data.id", ".data.error.message"},
	"permission.asked":              {".data.id", ".data.source.id", ".data.action", ".data.resources", ".data.metadata"},
	"permission.replied":            {".data.requestID"},
	"form.created":                  {".data.form.id", ".data.form.title", ".data.form.fields", ".data.form.metadata.kind", ".data.form.metadata.tool.id"},
	"form.replied":                  {".data.id", ".data.answer"},
	"form.cancelled":                {".data.id"},
	"session.execution.failed":      {".data.error.message"}, // url・ヘッダ・response.body は載せない
	"session.execution.interrupted": {".data.reason"},
}

// ownWords は、アダプタ自身が data に書く固定の語 (名前・状態・outcome)。frame に同じ値があっても、漏れではない。
var ownWords = map[string]bool{
	Name: true, v0.ToolPending: true, v0.ToolInProgress: true, v0.ToolCompleted: true, v0.ToolFailed: true,
	v0.AllowOnce: true, v0.AllowAlways: true, v0.RejectOnce: true, "always": true, "reject": true, "once": true,
	v0.FormAnswered: true, v0.FormCancelled: true,
}

func allowed(typ, path string) bool {
	for _, p := range allowedPaths[typ] {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

func TestDataDoesNotCarryUnintendedFrameValues(t *testing.T) {
	for _, scene := range scenes(t) {
		for i, line := range readLines(t, scene, "events.ndjson") {
			var f map[string]any
			if err := json.Unmarshal(line, &f); err != nil {
				t.Fatal(err)
			}
			typ, _ := f["type"].(string)
			k, ok := eventKinds[typ]
			if !ok {
				t.Fatalf("%s 行 %d: %s の扱いが無い", scene, i+1, typ)
			}
			// data を出す type は、allowedPaths が要る (何も許さない type は、表に無くてよい)。
			if _, has := allowedPaths[typ]; !has && k != kHousekeeping && k != kFrame && k != kToolInputStarted && k != kStepEnded && k != kExecSucceeded {
				t.Fatalf("%s 行 %d: %s の allowedPaths が無い。type が増えたら、表に足す", scene, i+1, typ)
			}
		}
		checkScene(t, scene)
	}
}

// checkScene は、場面を frame ごとに再生し、各 frame が出した Envelope の data に、許していない frame の値が出ないことを確かめる。
func checkScene(t *testing.T, scene string) {
	t.Helper()
	lines := readLines(t, scene, "events.ndjson")
	res := replayPerFrame(t, scene)
	for i, line := range lines {
		var f map[string]any
		_ = json.Unmarshal(line, &f)
		typ, _ := f["type"].(string)
		var ls []leaf
		leaves(f, "", &ls)
		var ok, bad []leaf
		for _, l := range ls {
			if allowed(typ, l.path) {
				ok = append(ok, l)
			} else if len(l.value) >= 4 && !ownWords[l.value] { // "edit" のような短い値と、アダプタ自身が書く語は、偶然の一致になる
				bad = append(bad, l)
			}
		}
		for _, e := range res[i] {
			var d any
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("%s の data が JSON でない: %s", e.Type, e.Data)
			}
			var dl []leaf
			leaves(d, "", &dl)
			for _, l := range bad {
				for _, x := range dl {
					if strings.Contains(x.value, l.value) && !containedInAllowed(x.value, l.value, ok) {
						t.Errorf("%s 行 %d (%s): frame の %s = %q が、%s の data に出た: %s", scene, i+1, typ, l.path, l.value, e.Type, e.Data)
					}
				}
			}
		}
	}
}

// containedInAllowed は、data の値 x が l を含む理由が、許した値 (ok) が l を含むから、だけかを返す
// (例: cwd の /work が、input の /work/hello.txt に含まれる)。
func containedInAllowed(x, l string, ok []leaf) bool {
	for _, a := range ok {
		if strings.Contains(a.value, l) && strings.Contains(x, a.value) {
			return true
		}
	}
	return false
}

// TestDataKeysAreFixed は、data のキーが、語彙の名前だけであることを固定する。値の漏れを、キーの側から見る。
func TestDataKeysAreFixed(t *testing.T) {
	want := map[string][]string{
		v0.TypeSessionStarted:      {"agent", "agent_session", "cwd", "version"},
		v0.TypeTurnStarted:         {"text"},
		v0.TypeMessageText:         {"message_id", "text"},
		v0.TypeToolCall:            {"call_id", "input", "kind", "name", "status"},
		v0.TypeToolUpdate:          {"call_id", "status"}, // completed は output (と、あれば exit)・failed は error が加わる
		v0.TypePermissionRequested: {"call_id", "details", "input", "kind", "request_id", "summary", "tool_name"},
		v0.TypePermissionResolved:  {"by", "outcome", "request_id"},
		v0.TypeFormRequested:       {"fields", "kind", "request_id", "title"}, // call_id は、あれば
		v0.TypeFormResolved:        {"by", "outcome", "request_id"},           // answered は answer が加わる
		v0.TypeUsage: {"cache_creation_input_tokens", "cache_read_input_tokens", "cost_usd", "input_tokens", "output_tokens",
			"reasoning_tokens", "scope"},
		v0.TypeError:         {"message", "retryable", "status"},
		v0.TypeTurnCompleted: {"is_error", "stop_reason"}, // cancelled は reason が加わる
		v0.TypeAgentFrame:    nil,
	}
	extra := map[string][]string{
		v0.TypeToolUpdate + "/" + v0.ToolCompleted:    {"output"},
		v0.TypeToolUpdate + "/" + v0.ToolFailed:       {"error"},
		v0.TypeFormResolved + "/" + v0.FormAnswered:   {"answer"},
		v0.TypeTurnCompleted + "/" + v0.StopCancelled: {"reason"},
	}
	optional := map[string]bool{"exit": true, "call_id": false}
	seen := map[string]bool{}
	for _, scene := range scenes(t) {
		for _, e := range decodeAll(t, scene) {
			seen[e.Type] = true
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil && len(e.Data) > 0 {
				t.Fatal(err)
			}
			w, ok := want[e.Type]
			if !ok {
				t.Fatalf("%s: 想定外の type", e.Type)
			}
			w = append([]string(nil), w...)
			for _, sub := range []any{m["status"], m["outcome"], m["stop_reason"]} {
				if s, ok := sub.(string); ok {
					w = append(w, extra[e.Type+"/"+s]...)
				}
			}
			if e.Type == v0.TypePermissionRequested && m["details"] == nil { // details が空 (resources が無い) の要求
				w = remove(w, "details")
			}
			var keys []string
			for k := range m {
				if !optional[k] && !(e.Type == v0.TypeFormRequested && k == "call_id") {
					keys = append(keys, k)
				}
			}
			if !sameSet(keys, w) {
				t.Errorf("%s の data のキー = %v, want %v", e.Type, keys, w)
			}
		}
	}
	for typ := range want {
		if !seen[typ] && typ != v0.TypeAgentFrame {
			t.Errorf("%s が、golden から出なかった", typ)
		}
	}
}

func remove(a []string, s string) []string {
	var out []string
	for _, x := range a {
		if x != s {
			out = append(out, x)
		}
	}
	return out
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

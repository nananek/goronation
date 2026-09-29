package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// readLines は、golden fixtures の claude/<name>.jsonl の各行を返す。
func readLines(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "testdata", "golden", "claude", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		out = append(out, bytes.Clone(sc.Bytes()))
	}
	if err := sc.Err(); err != nil || len(out) == 0 {
		t.Fatalf("%s: %v (%d 行)", name, err, len(out))
	}
	return out
}

// decodeAll は、fixture の全行を、1 つの Stream に流す。
func decodeAll(t *testing.T, name string) []v0.Envelope {
	t.Helper()
	s := Adapter{}.NewStream()
	var out []v0.Envelope
	for i, line := range readLines(t, name) {
		es, err := s.DecodeFrame(line)
		if err != nil {
			t.Fatalf("%s 行 %d: %v", name, i+1, err)
		}
		out = append(out, es...)
	}
	return out
}

func types(es []v0.Envelope) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

func data(t *testing.T, e v0.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("%s の data が JSON のオブジェクトでない: %s", e.Type, e.Data)
	}
	return m
}

func wantTypes(t *testing.T, es []v0.Envelope, want ...string) {
	t.Helper()
	if got := types(es); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("type の並び = %v, want %v", got, want)
	}
}

func TestSimpleText(t *testing.T) {
	es := decodeAll(t, "simple-text")
	wantTypes(t, es, v0.TypeSessionStarted, v0.TypeMessageText, v0.TypeUsage, v0.TypeTurnCompleted)
	if d := data(t, es[0]); d["agent"] != "claude" || d["agent_session"] != "<uuid:1>" || d["cwd"] != "/work" {
		t.Errorf("session.started の data = %v", d)
	}
	if d := data(t, es[1]); d["message_id"] != "msg_fake_000" || d["text"] != "こんにちは。フレーム採取用の fake サーバーです。" {
		t.Errorf("message.text の data = %v", d)
	}
	u := data(t, es[2])
	if u["scope"] != "turn" || u["input_tokens"] != 10.0 || u["output_tokens"] != 17.0 || u["context_window"] != 1000000.0 || u["max_output_tokens"] != 128000.0 {
		t.Errorf("usage の data = %v", u)
	}
	if d := data(t, es[3]); d["stop_reason"] != v0.StopEndTurn || d["is_error"] != false {
		t.Errorf("turn.completed の data = %v", d)
	}
	for _, e := range es {
		if e.V != v0.Version || e.ID != "" || e.TS != "" || e.Session != "" || e.Seq != 0 {
			t.Errorf("%s: V・ID・TS・Session・Seq が、契約と違う: %+v", e.Type, e)
		}
		if !e.Durable {
			t.Errorf("%s が durable でない", e.Type)
		}
		if len(e.Raw) == 0 || !json.Valid(e.Raw) {
			t.Errorf("%s の raw が、元のフレームでない: %s", e.Type, e.Raw)
		}
	}
}

func TestToolCall(t *testing.T) {
	es := decodeAll(t, "tool-call")
	wantTypes(t, es, v0.TypeSessionStarted, v0.TypeMessageText, v0.TypeToolCall, v0.TypeToolUpdate, v0.TypeMessageText, v0.TypeUsage, v0.TypeTurnCompleted)
	call, upd := data(t, es[2]), data(t, es[3])
	if call["call_id"] != "toolu_fake_1" || call["name"] != "Read" || call["kind"] != v0.KindRead || call["status"] != v0.ToolInProgress {
		t.Errorf("tool.call の data = %v", call)
	}
	if in, _ := call["input"].(map[string]any); in["file_path"] != "/work/hello.txt" {
		t.Errorf("tool.call の input が、書き換わった: %v", call["input"])
	}
	if upd["call_id"] != call["call_id"] || upd["status"] != v0.ToolCompleted || upd["output"] != "1\thello from the work directory\n2\t" {
		t.Errorf("tool.update の data = %v", upd)
	}
	// tool_use_result (ファイルの path と内容を持つ、frame 固有の付属物) は、data に出さない。
	if _, ok := upd["file"]; ok || strings.Contains(string(es[3].Data), "filePath") {
		t.Errorf("tool.update の data に、tool_use_result が出た: %s", es[3].Data)
	}
	// 2 つの message.text は、別の message.id
	if data(t, es[1])["message_id"] == data(t, es[4])["message_id"] {
		t.Error("別のメッセージが、同じ message_id になった")
	}
}

func TestMultiTurn(t *testing.T) {
	es := decodeAll(t, "multi-turn")
	wantTypes(t, es,
		v0.TypeSessionStarted, v0.TypeMessageText, v0.TypeUsage, v0.TypeTurnCompleted,
		v0.TypeAgentFrame, v0.TypeMessageText, v0.TypeUsage, v0.TypeTurnCompleted)
	if es[4].Durable || string(es[4].Data) != "{}" || !bytes.Contains(es[4].Raw, []byte(`"subtype":"init"`)) {
		t.Errorf("2 回目の init が、data 空・durable でない・raw に元のフレーム、になっていない: %+v", es[4])
	}
	// 別の起動 (Stream) の 1 回目は、また session.started
	if es2 := decodeAll(t, "simple-text"); es2[0].Type != v0.TypeSessionStarted {
		t.Error("新しい Stream の init が、session.started でない")
	}
}

// permission-request: 実際の system/permission_denied は message が文字列。拒否は permission.resolved として明示され、
// tool.update は failed、ターンは (claude の result どおり) is_error=false で終わる。拒否が成功に見えないことを固定する。
func TestPermissionDenied(t *testing.T) {
	es := decodeAll(t, "permission-request")
	wantTypes(t, es, v0.TypeSessionStarted, v0.TypeMessageText, v0.TypeToolCall, v0.TypePermissionResolved, v0.TypeToolUpdate,
		v0.TypeMessageText, v0.TypeUsage, v0.TypeTurnCompleted)
	d := data(t, es[3])
	if d["by"] != "policy" || d["outcome"] != v0.RejectOnce || d["call_id"] != "toolu_fake_1" || d["tool_name"] != "Write" || !es[3].Durable {
		t.Errorf("permission.resolved の data = %v", d)
	}
	if d := data(t, es[4]); d["status"] != v0.ToolFailed || d["call_id"] != "toolu_fake_1" {
		t.Errorf("tool.update の data = %v", d)
	}
	if d := data(t, es[2]); d["call_id"] != d2(es[3])["call_id"] {
		t.Errorf("tool.call と permission.resolved の call_id が合わない")
	}
}

// error-response: API の失敗 (HTTP 400)。assistant の API Error は agent.frame にして、result から error・usage・turn.completed を出す
// (status は result にだけある)。subtype は success のままなので、is_error を見ていることも固定する。
func TestErrorResponse(t *testing.T) {
	es := decodeAll(t, "error-response")
	wantTypes(t, es, v0.TypeSessionStarted, v0.TypeAgentFrame, v0.TypeError, v0.TypeUsage, v0.TypeTurnCompleted)
	d := data(t, es[2])
	if d["status"] != float64(400) || d["retryable"] != false || d["message"] != "API Error: 400 fake: このリクエストは受け付けられません" || !es[2].Durable {
		t.Errorf("error の data = %v", d)
	}
	if d := data(t, es[4]); d["is_error"] != true || d["stop_reason"] != v0.StopError {
		t.Errorf("turn.completed の data = %v", d)
	}
}

func TestErrorWithoutStatus(t *testing.T) {
	es, err := Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"result","subtype":"success","is_error":true,"terminal_reason":"aborted"}`))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, es, v0.TypeError, v0.TypeUsage, v0.TypeTurnCompleted)
	if d := data(t, es[0]); d["status"] != nil || d["retryable"] != false || d["message"] != "aborted" {
		t.Errorf("error の data = %v", d)
	}
}

func TestRetryable(t *testing.T) {
	i := func(n int) *int { return &n }
	for status, want := range map[*int]bool{nil: false, i(400): false, i(401): false, i(429): true, i(500): true, i(529): true} {
		if got := retryable(status); got != want {
			t.Errorf("retryable(%v) = %v, want %v", status, got, want)
		}
	}
}

// result のフレームの各フィールドが、想定外の型でも、フレームは (error・usage・turn.completed も) raw ごと失われない。
// (permission_denied の message と同じ、型の不一致で frame 全体の Unmarshal が失敗するバグの再発防止。攻撃者視点レビュー 43a56fa)
func TestResultFrameFieldTypeMismatchIsNotLost(t *testing.T) {
	cases := []string{
		`{"type":"result","is_error":true,"api_error_status":"500","result":"x"}`,
		`{"type":"result","is_error":true,"api_error_status":500.5,"result":"x"}`,
		`{"type":"result","is_error":true,"api_error_status":{"code":500},"result":"x"}`,
		`{"type":"result","is_error":true,"result":123}`,
		`{"type":"result","is_error":true,"terminal_reason":123}`,
		`{"type":"result","is_error":"true"}`,
		`{"type":"system","subtype":"init","session_id":1,"tools":"x","cwd":[]}`,
		`{"type":"system","subtype":"permission_denied","tool_name":1,"tool_use_id":{}}`,
		`{"type":"result","usage":"x","modelUsage":[],"total_cost_usd":"x","stop_reason":1}`,
	}
	for _, line := range cases {
		t.Run(line, func(t *testing.T) {
			envs, err := (&Stream{}).DecodeFrame([]byte(line))
			if err != nil || len(envs) == 0 {
				t.Errorf("フレームが失われた: envs=%d err=%v", len(envs), err)
			}
			for _, e := range envs {
				if len(e.Raw) == 0 {
					t.Errorf("%s の raw が空", e.Type)
				}
			}
		})
	}
}

func d2(e v0.Envelope) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(e.Data, &m)
	return m
}

func TestUnknownFramesAreKept(t *testing.T) {
	s := Adapter{}.NewStream()
	for _, line := range []string{
		`{"type":"rate_limit_event"}`,
		`{"type":"system","subtype":"something_new"}`,
		`{"type":"assistant","message":{"id":"m","content":[]}}`,
		`{"type":"assistant","message":{"id":"m","content":[{"type":"thinking","thinking":"x"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"x"}]}}`,
		`{"type":"assistant","is_api_error_message":true,"message":{"id":"m","content":[{"type":"text","text":"x"}]}}`, // error は後続の result から出す
		`{"type":"assistant"}`,
		`{"type":"assistant","message":"x"}`,
		`{"type":"user","message":"x"}`,
		`{}`,
	} {
		es, err := s.DecodeFrame([]byte(line))
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if len(es) != 1 || es[0].Type != v0.TypeAgentFrame || es[0].Durable || string(es[0].Data) != "{}" || string(es[0].Raw) != line {
			t.Errorf("%s -> %+v", line, es)
		}
	}
}

func TestDecodeFrameRejectsNonJSON(t *testing.T) {
	for _, line := range []string{``, `not json`, `{"type":`, `[1]`} {
		if es, err := (&Stream{}).DecodeFrame([]byte(line)); err == nil {
			t.Errorf("%q: error が返らない: %+v", line, es)
		}
	}
}

func TestRawIsCopied(t *testing.T) {
	line := []byte(`{"type":"other"}`)
	es, _ := (&Stream{}).DecodeFrame(line)
	line[2] = 'X' // 呼び手が、読み取りのバッファを使い回しても、raw は変わらない
	if string(es[0].Raw) != `{"type":"other"}` {
		t.Errorf("raw が、呼び手のバッファを共有している: %s", es[0].Raw)
	}
}

func TestToolKind(t *testing.T) {
	for name, want := range map[string]string{
		"Read": v0.KindRead, "Write": v0.KindEdit, "Edit": v0.KindEdit, "MultiEdit": v0.KindEdit, "NotebookEdit": v0.KindEdit,
		"Grep": v0.KindSearch, "Glob": v0.KindSearch, "WebSearch": v0.KindSearch,
		"Bash": v0.KindExecute, "WebFetch": v0.KindFetch,
		"Task": v0.KindOther, "mcp__srv__tool": v0.KindOther, "": v0.KindOther, "read": v0.KindOther,
	} {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEncodePrompt(t *testing.T) {
	cmd := v0.Command{V: v0.Version, Type: v0.CommandPrompt, Data: json.RawMessage(`{"text":"hi <b> \"q\"\n"}`)}
	raw, synth, err := (&Stream{}).EncodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) || bytes.Count(raw, []byte("\n")) != 1 {
		t.Fatalf("1 行 (末尾に改行 1 つ) でない: %q", raw)
	}
	var got struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "user" || got.Message.Role != "user" || len(got.Message.Content) != 1 ||
		got.Message.Content[0].Type != "text" || got.Message.Content[0].Text != "hi <b> \"q\"\n" {
		t.Errorf("stdin の 1 行 = %s", raw)
	}
	if len(synth) != 1 || synth[0].Type != v0.TypeTurnStarted || !synth[0].Durable || string(synth[0].Data) != "{}" || synth[0].Raw != nil || synth[0].V != v0.Version {
		t.Errorf("synthesized = %+v", synth)
	}
}

func TestEncodeRejects(t *testing.T) {
	for name, cmd := range map[string]v0.Command{
		"cancel (未対応)":     {Type: v0.CommandCancel, Data: json.RawMessage(`{}`)},
		"permission (未対応)": {Type: v0.CommandPermissionResolve, Data: json.RawMessage(`{}`)},
		"未知の type":         {Type: "x", Data: json.RawMessage(`{}`)},
		"data が JSON でない":  {Type: v0.CommandPrompt, Data: json.RawMessage(`nope`)},
		"data が無い":         {Type: v0.CommandPrompt},
		"text が空":          {Type: v0.CommandPrompt, Data: json.RawMessage(`{"text":""}`)},
		"text が無い":         {Type: v0.CommandPrompt, Data: json.RawMessage(`{}`)},
	} {
		if raw, synth, err := (&Stream{}).EncodeCommand(cmd); err == nil || raw != nil || synth != nil {
			t.Errorf("%s: error にならない: %q %+v", name, raw, synth)
		}
	}
}

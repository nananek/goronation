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

// 零値が反対の意味を持つフラグは、型が違っても、成功・通常の発言に化けない (fail-safe)。
func TestMalformedFlagsFailSafe(t *testing.T) {
	es, err := Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"result","is_error":"true","api_error_status":400,"result":"API Error: 400 x"}`))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, es, v0.TypeError, v0.TypeUsage, v0.TypeTurnCompleted)
	if d := data(t, es[2]); d["is_error"] != true || d["stop_reason"] != v0.StopError {
		t.Errorf("turn.completed の data = %v", d)
	}
	es, err = Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"assistant","is_api_error_message":"true","message":{"id":"m","content":[{"type":"text","text":"API Error: 400 x"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, es, v0.TypeAgentFrame)
	// 重複したキーは、最後の値が読めなければ零値 (先の値を残さない)
	es, _ = Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"result","is_error":false,"is_error":"x"}`))
	wantTypes(t, es, v0.TypeError, v0.TypeUsage, v0.TypeTurnCompleted)
}

// null は、bool へはエラーなしの no-op なので、Bad にしないと、is_error・is_api_error_message の fail-safe をすり抜ける。
func TestNullFlagsFailSafe(t *testing.T) {
	es, err := Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"result","is_error":null,"api_error_status":400,"result":"API Error: 400 x"}`))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, es, v0.TypeError, v0.TypeUsage, v0.TypeTurnCompleted)
	es, err = Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"assistant","is_api_error_message":null,"message":{"id":"m","content":[{"type":"text","text":"API Error: 400 x"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, es, v0.TypeAgentFrame)
	// 重複したキーの最後が null でも、同じ
	es, _ = Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"result","is_error":false,"is_error":null}`))
	wantTypes(t, es, v0.TypeError, v0.TypeUsage, v0.TypeTurnCompleted)
	// キーが無いのは、成功 (null とは違う)
	es, _ = Adapter{}.NewStream().DecodeFrame([]byte(`{"type":"result"}`))
	wantTypes(t, es, v0.TypeUsage, v0.TypeTurnCompleted)
}

// tool_result の is_error も、null・型の違いで成功に化けず、同じ message の別の tool_result も巻き添えにしない。
func TestToolResultIsErrorFailSafe(t *testing.T) {
	for _, bad := range []string{`null`, `"true"`, `0`, `[]`} {
		line := []byte(`{"type":"user","message":{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_good","is_error":false,"content":"ok output"},
			{"type":"tool_result","tool_use_id":"toolu_bad","is_error":` + bad + `,"content":"boom"}]}}`)
		es, err := Adapter{}.NewStream().DecodeFrame(line)
		if err != nil {
			t.Fatal(err)
		}
		wantTypes(t, es, v0.TypeToolUpdate, v0.TypeToolUpdate)
		if d := data(t, es[0]); d["call_id"] != "toolu_good" || d["status"] != v0.ToolCompleted || d["output"] != "ok output" {
			t.Errorf("is_error=%s: 正常な tool_result の data = %v", bad, d)
		}
		if d := data(t, es[1]); d["call_id"] != "toolu_bad" || d["status"] != v0.ToolFailed || d["error"] != "boom" {
			t.Errorf("is_error=%s: 読めない is_error の tool_result の data = %v", bad, d)
		}
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
	s2 := &Stream{}
	raw, synth, err := s2.EncodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if len(lines) != 3 || len(lines[2]) != 0 || !bytes.HasSuffix(lines[1], []byte("\n")) {
		t.Fatalf("initialize + prompt の 2 行 (それぞれ改行で終わる) でない: %q", raw)
	}
	if string(lines[0]) != `{"request":{"subtype":"initialize"},"request_id":"goronation-init","type":"control_request"}`+"\n" {
		t.Errorf("1 行目 (initialize) = %s", lines[0])
	}
	raw = lines[1] // 以下は prompt の行
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
	// 2 回目の prompt には、initialize を付けない。
	raw2, _, err := s2.EncodeCommand(cmd)
	if err != nil || bytes.Count(raw2, []byte("\n")) != 1 {
		t.Errorf("2 回目の prompt = %q, %v", raw2, err)
	}
	if len(synth) != 1 || synth[0].Type != v0.TypeTurnStarted || !synth[0].Durable || string(synth[0].Data) != `{"text":"hi <b> \"q\"\n"}` || synth[0].Raw != nil || synth[0].V != v0.Version {
		t.Errorf("synthesized = %+v", synth)
	}
}

func TestEncodeRejects(t *testing.T) {
	for name, cmd := range map[string]v0.Command{
		"cancel (未対応)":        {Type: v0.CommandCancel, Data: json.RawMessage(`{}`)},
		"permission (未知の ID)": {Type: v0.CommandPermissionResolve, Data: json.RawMessage(`{"request_id":"nope","outcome":"allow_once"}`)},
		"未知の type":            {Type: "x", Data: json.RawMessage(`{}`)},
		"data が JSON でない":     {Type: v0.CommandPrompt, Data: json.RawMessage(`nope`)},
		"data が無い":            {Type: v0.CommandPrompt},
		"text が空":             {Type: v0.CommandPrompt, Data: json.RawMessage(`{"text":""}`)},
		"text が無い":            {Type: v0.CommandPrompt, Data: json.RawMessage(`{}`)},
	} {
		if raw, synth, err := (&Stream{}).EncodeCommand(cmd); err == nil || raw != nil || synth != nil {
			t.Errorf("%s: error にならない: %q %+v", name, raw, synth)
		}
	}
}

// --- 対話の権限要求 (PR⓪ の fixtures: claude 2.1.284、--permission-prompt-tool stdio) ---

func resolveCmd(id, outcome string) v0.Command {
	return v0.Command{V: v0.Version, Type: v0.CommandPermissionResolve, Data: json.RawMessage(`{"request_id":"` + id + `","outcome":"` + outcome + `"}`)}
}

// decodeUntilRequest は、fixture を、最初の permission.requested まで流す。
func decodeUntilRequest(t *testing.T, name string) (*Stream, v0.Envelope) {
	t.Helper()
	s := Adapter{}.NewStream()
	for _, line := range readLines(t, name) {
		es, err := s.DecodeFrame(line)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Type == v0.TypePermissionRequested {
				return s.(*Stream), e
			}
		}
	}
	t.Fatalf("%s: permission.requested が出ない", name)
	return nil, v0.Envelope{}
}

func TestInteractiveAllowFlow(t *testing.T) {
	// 実際の流れ: 要求が来たら、goronation が allow_once で答える (答えた要求は、result で撤回されない)。
	s := Adapter{}.NewStream()
	var es []v0.Envelope
	for _, line := range readLines(t, "permission-interactive-allow") {
		got, err := s.DecodeFrame(line)
		if err != nil {
			t.Fatal(err)
		}
		es = append(es, got...)
		for _, e := range got {
			if e.Type == v0.TypePermissionRequested {
				if _, _, err := s.EncodeCommand(resolveCmd(data(t, e)["request_id"].(string), v0.AllowOnce)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	wantTypes(t, es,
		v0.TypeAgentFrame, // initialize の control_response
		v0.TypeSessionStarted,
		v0.TypeMessageText, v0.TypeToolCall,
		v0.TypePermissionRequested,
		v0.TypeToolUpdate, v0.TypeMessageText, v0.TypeUsage, v0.TypeTurnCompleted,
		v0.TypeAgentFrame, // 2 ターン目の system/init
		v0.TypeMessageText, v0.TypeUsage, v0.TypeTurnCompleted)
	d := data(t, es[4])
	if d["request_id"] != "<uuid:5>" || d["call_id"] != "toolu_fake_1" || d["tool_name"] != "Write" || d["kind"] != v0.KindEdit || d["title"] != "new.txt" {
		t.Errorf("permission.requested の data = %v", d)
	}
	in, _ := d["input"].(map[string]any)
	if in["file_path"] != "/work/new.txt" || in["content"] != "new file\n" {
		t.Errorf("input が、書き換えずに載っていない: %v", d["input"])
	}
	if es[4].Durable != true || es[4].Raw == nil {
		t.Errorf("durable・raw = %v %s", es[4].Durable, es[4].Raw)
	}
}

func TestInteractiveResolveAllowUsesHeldInput(t *testing.T) {
	s, req := decodeUntilRequest(t, "permission-interactive-allow")
	raw, synth, err := s.EncodeCommand(resolveCmd("<uuid:5>", v0.AllowOnce))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"response":{"request_id":"<uuid:5>","response":{"behavior":"allow","updatedInput":{"content":"new file\n","file_path":"/work/new.txt"}},"subtype":"success"},"type":"control_response"}` + "\n"
	if string(raw) != want {
		t.Errorf("control_response =\n%s want\n%s", raw, want)
	}
	if len(synth) != 1 || synth[0].Type != v0.TypePermissionResolved || !synth[0].Durable || synth[0].Raw != nil ||
		string(synth[0].Data) != `{"by":"human","outcome":"allow_once","request_id":"<uuid:5>"}` {
		t.Errorf("synthesized = %+v", synth)
	}
	_ = req
	// 1 回限り: 2 回目 (別タブの後追いなど) は、何も書かず error。
	if raw, synth, err := s.EncodeCommand(resolveCmd("<uuid:5>", v0.AllowOnce)); err == nil || raw != nil || synth != nil {
		t.Errorf("2 回目の応答が通った: %q %+v", raw, synth)
	}
	if _, _, err := s.EncodeCommand(resolveCmd("<uuid:5>", v0.RejectOnce)); err == nil {
		t.Error("応答済みの要求への、逆の応答が通った")
	}
}

func TestInteractiveResolveReject(t *testing.T) {
	s, _ := decodeUntilRequest(t, "permission-interactive-deny")
	raw, synth, err := s.EncodeCommand(resolveCmd("<uuid:5>", v0.RejectOnce))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"response":{"request_id":"<uuid:5>","response":{"behavior":"deny","message":"The user denied this tool use."},"subtype":"success"},"type":"control_response"}` + "\n"
	if string(raw) != want || len(synth) != 1 || string(synth[0].Data) != `{"by":"human","outcome":"reject_once","request_id":"<uuid:5>"}` {
		t.Errorf("raw = %s synth = %+v", raw, synth)
	}
}

func TestInteractiveResolveRejects(t *testing.T) {
	s, _ := decodeUntilRequest(t, "permission-interactive-allow")
	for name, cmd := range map[string]v0.Command{
		"未知の ID":          resolveCmd("other", v0.AllowOnce),
		"allow_always":    resolveCmd("<uuid:5>", v0.AllowAlways),
		"reject_always":   resolveCmd("<uuid:5>", v0.RejectAlways),
		"未知の outcome":     resolveCmd("<uuid:5>", "allow"),
		"outcome が無い":     {Type: v0.CommandPermissionResolve, Data: json.RawMessage(`{"request_id":"<uuid:5>"}`)},
		"data が JSON でない": {Type: v0.CommandPermissionResolve, Data: json.RawMessage(`x`)},
	} {
		if raw, synth, err := s.EncodeCommand(cmd); err == nil || raw != nil || synth != nil {
			t.Errorf("%s: 通った: %q %+v", name, raw, synth)
		}
	}
	// 失敗した応答は、未決を消さない (その後の正しい応答は通る)。
	if _, _, err := s.EncodeCommand(resolveCmd("<uuid:5>", v0.AllowOnce)); err != nil {
		t.Errorf("失敗した応答の後に、正しい応答が通らない: %v", err)
	}
}

func TestInteractiveResolveIgnoresClientInput(t *testing.T) {
	// クライアントが data に input・message を足しても、control_response には出ない。
	s, _ := decodeUntilRequest(t, "permission-interactive-allow")
	cmd := v0.Command{Type: v0.CommandPermissionResolve, Data: json.RawMessage(`{"request_id":"<uuid:5>","outcome":"allow_once","input":{"file_path":"/etc/passwd"},"message":"x"}`)}
	raw, _, err := s.EncodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "passwd") || strings.Contains(string(raw), `"x"`) {
		t.Errorf("クライアントの値が、control_response に出た: %s", raw)
	}
}

func TestInteractiveCancelledByAgent(t *testing.T) {
	es := decodeAll(t, "permission-interactive-interrupt")
	var got []v0.Envelope
	for _, e := range es {
		if e.Type == v0.TypePermissionRequested || e.Type == v0.TypePermissionResolved {
			got = append(got, e)
		}
	}
	if len(got) != 2 || got[0].Type != v0.TypePermissionRequested || string(got[1].Data) != `{"by":"agent","outcome":"cancelled","request_id":"<uuid:5>"}` {
		t.Fatalf("要求と撤回 = %+v", got)
	}
	last := es[len(es)-1]
	if last.Type != v0.TypeTurnCompleted || data(t, last)["is_error"] != true {
		t.Errorf("最後 = %+v", last)
	}
	// 撤回された要求には、もう応答できない。
	s, _ := decodeUntilRequest(t, "permission-interactive-interrupt")
	if _, err := s.DecodeFrame(readLines(t, "permission-interactive-interrupt")[5]); err != nil { // control_cancel_request
		t.Fatal(err)
	}
	if _, _, err := s.EncodeCommand(resolveCmd("<uuid:5>", v0.AllowOnce)); err == nil {
		t.Error("撤回済みの要求に応答できた")
	}
}

func TestInteractiveDenyFlowKeepsToolFailed(t *testing.T) {
	es := decodeAll(t, "permission-interactive-deny")
	for _, e := range es {
		if e.Type == v0.TypeToolUpdate {
			if d := data(t, e); d["status"] != v0.ToolFailed || d["error"] != "このファイルへの書き込みは却下します。" {
				t.Errorf("tool.update = %v", d)
			}
			return
		}
	}
	t.Error("tool.update が出ない")
}

func TestPendingClosedAtTurnEnd(t *testing.T) {
	// 未決のまま result が来たら、撤回として閉じてから turn.completed を出す。
	s := Adapter{}.NewStream()
	for _, l := range []string{
		`{"type":"control_request","request_id":"a","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t1","input":{"command":"ls"}}}`,
		`{"type":"control_request","request_id":"b","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t2","input":{"command":"pwd"}}}`,
	} {
		if es, err := s.DecodeFrame([]byte(l)); err != nil || len(es) != 1 || es[0].Type != v0.TypePermissionRequested {
			t.Fatalf("%s -> %+v %v", l, es, err)
		}
	}
	es, err := s.DecodeFrame([]byte(`{"type":"result","is_error":false}`))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, es, v0.TypePermissionResolved, v0.TypePermissionResolved, v0.TypeUsage, v0.TypeTurnCompleted)
	if d := data(t, es[0]); d["request_id"] != "a" || d["outcome"] != "cancelled" || d["by"] != "agent" {
		t.Errorf("resolved[0] = %v", d)
	}
	if d := data(t, es[1]); d["request_id"] != "b" {
		t.Errorf("resolved[1] = %v", d)
	}
	if _, _, err := s.(*Stream).EncodeCommand(resolveCmd("a", v0.AllowOnce)); err == nil {
		t.Error("閉じた要求に応答できた")
	}
}

func TestControlFramesAreLenient(t *testing.T) {
	s := Adapter{}.NewStream()
	req := func(id, req string) string {
		return `{"type":"control_request","request_id":` + id + `,"request":` + req + `}`
	}
	ok := `{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t","input":{"command":"ls"}}`
	for _, line := range []string{
		req(`"x"`, `{"subtype":"something_new"}`),
		req(`"x"`, `{"subtype":"elicitation"}`),
		req(`"x"`, `"str"`),
		`{"type":"control_request","request":` + ok + `}`, // request_id が無い
		req(`""`, ok), // 空
		req(`7`, ok),  // 型が違う
		req(`"x"`, `{"subtype":"can_use_tool","tool_use_id":"t","input":{}}`),                      // tool_name が無い
		req(`"x"`, `{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t"}`),              // input が無い
		req(`"x"`, `{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t","input":"ls"}`), // input がオブジェクトでない
		req(`"x"`, `{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t","input":null}`),
		`{"type":"control_cancel_request"}`,
		`{"type":"control_cancel_request","request_id":"never-requested"}`, // 未決でない
		`{"type":"control_response","response":{"subtype":"success","request_id":"goronation-init"}}`,
	} {
		es, err := s.DecodeFrame([]byte(line))
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if len(es) != 1 || es[0].Type != v0.TypeAgentFrame || es[0].Durable || string(es[0].Data) != "{}" || string(es[0].Raw) != line {
			t.Errorf("%s -> %+v", line, es)
		}
	}
	// 応答できない形の要求は、未決に残らない (応答も通らない)。
	if _, _, err := s.(*Stream).EncodeCommand(resolveCmd("x", v0.AllowOnce)); err == nil {
		t.Error("agent.frame にした要求に応答できた")
	}
}

func TestDuplicateRequestIDDoesNotOverwrite(t *testing.T) {
	s := Adapter{}.NewStream()
	first := `{"type":"control_request","request_id":"a","request":{"subtype":"can_use_tool","tool_name":"Write","tool_use_id":"t","input":{"file_path":"/work/ok"}}}`
	second := `{"type":"control_request","request_id":"a","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t2","input":{"command":"rm -rf /"}}}`
	if es, _ := s.DecodeFrame([]byte(first)); len(es) != 1 || es[0].Type != v0.TypePermissionRequested {
		t.Fatalf("first -> %+v", es)
	}
	if es, _ := s.DecodeFrame([]byte(second)); len(es) != 1 || es[0].Type != v0.TypeAgentFrame {
		t.Fatalf("同じ ID の再要求が、agent.frame にならない: %+v", es)
	}
	raw, _, err := s.(*Stream).EncodeCommand(resolveCmd("a", v0.AllowOnce))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "/work/ok") || strings.Contains(string(raw), "rm -rf") {
		t.Errorf("後の要求の input で、上書きされた: %s", raw)
	}
}

// 採取した claude の版が、fixtures と一致すること (版を上げて採り直したら、TestedVersion と ADR 0010 も更新する)。
func TestTestedVersionMatchesFixtures(t *testing.T) {
	for _, name := range []string{"permission-interactive-allow", "permission-interactive-deny", "permission-interactive-interrupt"} {
		found := false
		for _, line := range readLines(t, name) {
			var f struct {
				Type, Subtype string
				Version       string `json:"claude_code_version"`
			}
			if json.Unmarshal(line, &f) == nil && f.Type == "system" && f.Subtype == "init" {
				found = true
				if f.Version != TestedVersion {
					t.Errorf("%s: claude_code_version = %q, TestedVersion = %q", name, f.Version, TestedVersion)
				}
			}
		}
		if !found {
			t.Errorf("%s: system/init が無い", name)
		}
	}
}

// FuzzDecodeAndResolve は、任意の行を流しても panic せず、返す封筒の data が JSON で raw が元の行であること、
// 未決の要求への応答が、要求時の input 以外を含まないことを確かめる。
func FuzzDecodeAndResolve(f *testing.F) {
	for _, l := range []string{
		`{"type":"control_request","request_id":"a","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t","input":{"command":"ls"}}}`,
		`{"type":"control_cancel_request","request_id":"a"}`,
		`{"type":"result","is_error":false}`,
		`{"type":"control_request","request_id":"a","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}`,
	} {
		f.Add([]byte(l), []byte(l))
	}
	f.Fuzz(func(t *testing.T, a, b []byte) {
		s := &Stream{}
		for _, line := range [][]byte{a, b} {
			es, err := s.DecodeFrame(line)
			if err != nil {
				continue
			}
			for _, e := range es {
				if !json.Valid(e.Data) || string(e.Raw) != string(line) {
					t.Fatalf("data = %q, raw = %q, line = %q", e.Data, e.Raw, line)
				}
				if e.Type != v0.TypePermissionRequested {
					continue
				}
				var d struct {
					RequestID string          `json:"request_id"`
					Input     json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(e.Data, &d); err != nil || d.RequestID == "" {
					t.Fatalf("permission.requested の data = %q", e.Data)
				}
				raw, _, err := s.EncodeCommand(resolveCmd(d.RequestID, v0.AllowOnce))
				if err != nil {
					continue // 同じ ID の再要求が先に来て、応答済み、など
				}
				var got struct {
					Response struct {
						Response struct {
							UpdatedInput json.RawMessage `json:"updatedInput"`
						} `json:"response"`
					} `json:"response"`
				}
				if json.Unmarshal(raw, &got) != nil || !json.Valid(got.Response.Response.UpdatedInput) {
					t.Fatalf("control_response = %q", raw)
				}
			}
		}
	})
}

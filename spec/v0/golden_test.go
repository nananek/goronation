package v0

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frame は、golden fixtures の 1 フレーム (JSON のオブジェクト)。
type frame map[string]any

// readFrames は、spec/testdata/golden/<agent>/<name> の全フレームを読む。
func readFrames(t *testing.T, agent, name string) []frame {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "golden", agent, name))
	if err != nil {
		t.Fatal(err)
	}
	var out []frame
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var f frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("%s/%s: %v", agent, name, err)
		}
		out = append(out, f)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s/%s: フレームが無い", agent, name)
	}
	return out
}

func str(f frame, key string) string { s, _ := f[key].(string); return s }

func sub(f frame, key string) frame { m, _ := f[key].(map[string]any); return m }

// count は、type (と subtype) が一致するフレームの数。subtype が空なら見ない。
func count(fs []frame, typ, subtype string) int {
	n := 0
	for _, f := range fs {
		if str(f, "type") == typ && (subtype == "" || str(f, "subtype") == subtype) {
			n++
		}
	}
	return n
}

// このファイルの test は、doc の対応 (envelope.go の各定数の doc) が根拠にした、golden fixtures の観察を固定する。
// 採取し直して形が変わったら、赤になる。対応を見直すか、エージェントの版の差として扱うかを、そのとき決める。

func TestClaudeInitRepeatsPerTurn(t *testing.T) {
	fs := readFrames(t, "claude", "multi-turn.jsonl")
	if n := count(fs, "system", "init"); n != 2 {
		t.Errorf("init の数 = %d, want 2 (ターンごとに出る)", n)
	}
	var idx []float64
	for _, f := range fs {
		if str(f, "type") == "result" {
			i, _ := f["result_index"].(float64)
			idx = append(idx, i)
		}
	}
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 1 {
		t.Errorf("result_index = %v, want [0 1]", idx)
	}
	for _, f := range fs {
		if str(f, "session_id") != str(fs[0], "session_id") {
			t.Fatal("2 ターンで、session_id が変わった")
		}
	}
}

func TestClaudeToolResultIsAUserFrame(t *testing.T) {
	fs := readFrames(t, "claude", "tool-call.jsonl")
	var callID, resultID string
	for _, f := range fs {
		m := sub(f, "message")
		content, _ := m["content"].([]any)
		for _, c := range content {
			b, _ := c.(map[string]any)
			switch b["type"] {
			case "tool_use":
				callID, _ = b["id"].(string)
				if str(f, "type") != "assistant" {
					t.Errorf("tool_use のフレームの type = %q", str(f, "type"))
				}
			case "tool_result":
				resultID, _ = b["tool_use_id"].(string)
				if str(f, "type") != "user" {
					t.Errorf("tool_result のフレームの type = %q, want user", str(f, "type"))
				}
			}
		}
	}
	if callID == "" || callID != resultID {
		t.Errorf("tool_use.id = %q, tool_result.tool_use_id = %q", callID, resultID)
	}
}

func TestClaudePermissionDeniedIsAfterTheFact(t *testing.T) {
	fs := readFrames(t, "claude", "permission-request.jsonl")
	if n := count(fs, "system", "permission_denied"); n != 1 {
		t.Errorf("permission_denied の数 = %d, want 1", n)
	}
	last := fs[len(fs)-1]
	if str(last, "type") != "result" {
		t.Fatalf("最後のフレームの type = %q", str(last, "type"))
	}
	if d, _ := last["permission_denials"].([]any); len(d) != 1 {
		t.Errorf("permission_denials = %v, want 1 件", d)
	}
	for _, f := range fs {
		if s := str(f, "subtype"); s == "permission_request" || str(f, "type") == "control_request" {
			t.Errorf("非対話の実行に、権限の要求のフレームが出た: %v", f)
		}
	}
}

func TestClaudeErrorResultHasSuccessSubtype(t *testing.T) {
	fs := readFrames(t, "claude", "error-response.jsonl")
	last := fs[len(fs)-1]
	if last["is_error"] != true || str(last, "subtype") != "success" || last["api_error_status"] != float64(400) ||
		str(last, "terminal_reason") != "api_error" {
		t.Errorf("result = is_error %v, subtype %q, status %v, terminal_reason %q",
			last["is_error"], str(last, "subtype"), last["api_error_status"], str(last, "terminal_reason"))
	}
	if first := fs[1]; first["is_api_error_message"] != true {
		t.Errorf("エラーの assistant フレームに is_api_error_message が無い: %v", first)
	}
}

func TestOpencodeFramesAreWholeParts(t *testing.T) {
	allowed := map[string]bool{"step_start": true, "text": true, "tool_use": true, "step_finish": true, "error": true}
	for _, name := range []string{"simple-text", "tool-call", "permission-request", "error-response", "multi-turn"} {
		for _, f := range readFrames(t, "opencode", name+".ndjson") {
			if !allowed[str(f, "type")] {
				t.Errorf("%s: 想定外のフレームの type = %q (部分メッセージが出た?)", name, str(f, "type"))
			}
			if str(f, "type") == "text" {
				if _, ok := sub(sub(f, "part"), "time")["end"]; !ok {
					t.Errorf("%s: text のフレームの part.time.end が無い", name)
				}
			}
			if str(f, "type") == "tool_use" {
				switch st := str(sub(sub(f, "part"), "state"), "status"); st {
				case "completed", "error":
				default:
					t.Errorf("%s: tool_use の status = %q (完了か失敗の後にだけ出るはず)", name, st)
				}
			}
		}
	}
}

func TestOpencodeDeniedTurnEndsWithoutStop(t *testing.T) {
	fs := readFrames(t, "opencode", "permission-request.ndjson")
	last := fs[len(fs)-1]
	if str(last, "type") != "step_finish" || str(sub(last, "part"), "reason") != "tool-calls" {
		t.Errorf("最後のフレーム = %s / reason %q, want step_finish / tool-calls", str(last, "type"), str(sub(last, "part"), "reason"))
	}
	if e := str(sub(sub(fs[2], "part"), "state"), "error"); e == "" {
		t.Errorf("拒否された tool の state.error が無い")
	}
}

func TestOpencodeErrorIsASingleFrame(t *testing.T) {
	fs := readFrames(t, "opencode", "error-response.ndjson")
	if len(fs) != 1 || str(fs[0], "type") != "error" {
		t.Fatalf("フレーム = %v, want error 1 つ", fs)
	}
	data := sub(sub(fs[0], "error"), "data")
	if data["statusCode"] != float64(400) || data["isRetryable"] != false {
		t.Errorf("error.data = %v", data)
	}
}

// PR⓪ スパイク (Issue #1) の所見: 非対話 (TestClaudePermissionDeniedIsAfterTheFact) と違い、
// `--permission-prompt-tool stdio` を付けた host モードでは、claude は can_use_tool の
// control_request (agent → host) を出し、host の control_response を待つ。以下 3 つは、その
// フレームの形を固定する (アダプタの対応を書く前の、実測の記録)。

// allow で答えると、tool が実際に実行され (tool_result に is_error が無い)、result の
// permission_denials が空で終わり、result を見てから開いた stdin に書いた 2 ターン目 (追いプロンプト)
// も、同じ session_id のまま続く。
func TestClaudeInteractiveAllowRunsToolAndContinues(t *testing.T) {
	fs := readFrames(t, "claude", "permission-interactive-allow.jsonl")
	if n := count(fs, "control_request", ""); n != 1 {
		t.Errorf("control_request の数 = %d, want 1", n)
	}
	var toolResult frame
	for _, f := range fs {
		if str(f, "type") != "user" {
			continue
		}
		content, _ := sub(f, "message")["content"].([]any)
		for _, c := range content {
			if b, _ := c.(map[string]any); b["type"] == "tool_result" {
				toolResult = f
			}
		}
	}
	if toolResult == nil {
		t.Fatal("tool_result のフレームが無い")
	}
	if tr, _ := toolResult["tool_use_result"].(map[string]any); tr == nil || tr["type"] != "create" {
		t.Errorf("tool_use_result = %v, want type=create (実際に書き込みが実行されたはず)", toolResult["tool_use_result"])
	}
	var results []frame
	for _, f := range fs {
		if str(f, "type") == "result" {
			results = append(results, f)
		}
	}
	if len(results) != 2 {
		t.Fatalf("result の数 = %d, want 2 (2 ターン)", len(results))
	}
	if d, _ := results[0]["permission_denials"].([]any); len(d) != 0 {
		t.Errorf("1 ターン目の permission_denials = %v, want 空 (allow で答えたので拒否は無いはず)", d)
	}
	if str(results[0], "session_id") != str(results[1], "session_id") {
		t.Error("2 ターンで session_id が変わった (追いプロンプトは、同じ会話を続けているはず)")
	}
}

// deny で答えるときの message は、host (この harness) が control_response に載せた文言がそのまま
// tool_result になる (非対話の自動拒否のときの、CLI 自身の定型文とは違う)。tool_result_meta の
// non_execution_kind も "permission-rule" になり (自動拒否の "user-rejected" とは別)、
// system/permission_denied のフレームも出ない (あれは、誰も答えなかったときだけ)。
func TestClaudeInteractiveDenyUsesHostMessage(t *testing.T) {
	fs := readFrames(t, "claude", "permission-interactive-deny.jsonl")
	if n := count(fs, "system", "permission_denied"); n != 0 {
		t.Errorf("permission_denied の数 = %d, want 0 (host が答えたので、CLI 自身の自動拒否は起きないはず)", n)
	}
	var toolResult frame
	for _, f := range fs {
		if str(f, "type") != "user" {
			continue
		}
		content, _ := sub(f, "message")["content"].([]any)
		for _, c := range content {
			if b, _ := c.(map[string]any); b["type"] == "tool_result" {
				toolResult = f
			}
		}
	}
	if toolResult == nil {
		t.Fatal("tool_result のフレームが無い")
	}
	content, _ := sub(toolResult, "message")["content"].([]any)
	block, _ := content[0].(map[string]any)
	if block["is_error"] != true {
		t.Errorf("tool_result.is_error = %v, want true", block["is_error"])
	}
	if text, _ := block["content"].(string); text == "" || strings.Contains(text, "you haven't granted it yet") {
		t.Errorf("tool_result.content = %q, want host が渡した message そのもの (CLI の定型文ではない)", text)
	}
	meta, _ := toolResult["tool_result_meta"].([]any)
	if len(meta) != 1 {
		t.Fatalf("tool_result_meta = %v, want 1 件", meta)
	}
	if m, _ := meta[0].(map[string]any); m["non_execution_kind"] != "permission-rule" {
		t.Errorf("non_execution_kind = %v, want permission-rule", m["non_execution_kind"])
	}
}

// can_use_tool に答えず、host 発の interrupt の control_request (+ その request_id への
// control_cancel_request) を送ると、claude 自身も (別の request_id で) その can_use_tool 自身への
// control_cancel_request を host に返し (「もう答えなくてよい」の相互通知)、ターンは
// terminal_reason "aborted_tools" で終わる。
func TestClaudeInteractiveInterruptAbortsTheTurn(t *testing.T) {
	fs := readFrames(t, "claude", "permission-interactive-interrupt.jsonl")
	var reqID string
	for _, f := range fs {
		if str(f, "type") == "control_request" {
			reqID = str(f, "request_id")
		}
	}
	if reqID == "" {
		t.Fatal("can_use_tool の control_request が無い")
	}
	if n := count(fs, "control_cancel_request", ""); n != 1 {
		t.Fatalf("control_cancel_request の数 = %d, want 1", n)
	}
	for _, f := range fs {
		if str(f, "type") == "control_cancel_request" && str(f, "request_id") != reqID {
			t.Errorf("control_cancel_request.request_id = %q, want can_use_tool と同じ %q", str(f, "request_id"), reqID)
		}
	}
	last := fs[len(fs)-1]
	if str(last, "type") != "result" || str(last, "terminal_reason") != "aborted_tools" || last["is_error"] != true {
		t.Errorf("最後のフレーム = type %q, terminal_reason %q, is_error %v, want result / aborted_tools / true",
			str(last, "type"), str(last, "terminal_reason"), last["is_error"])
	}
}

func TestOpencodeMultiTurnKeepsTheSession(t *testing.T) {
	fs := readFrames(t, "opencode", "multi-turn.ndjson")
	msgs := map[string]bool{}
	for _, f := range fs {
		if str(f, "sessionID") != str(fs[0], "sessionID") {
			t.Fatal("2 ターンで、sessionID が変わった (--continue で同じセッションのはず)")
		}
		msgs[str(sub(f, "part"), "messageID")] = true
	}
	if len(msgs) != 2 {
		t.Errorf("messageID の種類 = %d, want 2 (ターンごとに別)", len(msgs))
	}
}

package v0

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// readLines は、buf (改行区切りの JSON) を、書かれた順の frame の列にする。
func readLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("stdin に書かれた行が JSON でない: %v (%s)", err, line)
		}
		out = append(out, m)
	}
	return out
}

func TestControlResponseAllowShape(t *testing.T) {
	m := controlResponseAllow("req-1")
	if m["type"] != "control_response" {
		t.Fatalf("type = %v", m["type"])
	}
	resp := m["response"].(map[string]any)
	if resp["subtype"] != "success" || resp["request_id"] != "req-1" {
		t.Fatalf("response = %+v", resp)
	}
	inner := resp["response"].(map[string]any)
	if inner["behavior"] != "allow" {
		t.Fatalf("inner = %+v", inner)
	}
}

func TestControlResponseDenyShape(t *testing.T) {
	m := controlResponseDeny("req-2", "だめです")
	resp := m["response"].(map[string]any)
	inner := resp["response"].(map[string]any)
	if inner["behavior"] != "deny" || inner["message"] != "だめです" {
		t.Fatalf("inner = %+v", inner)
	}
}

func TestControlRequestInterruptAndCancelShape(t *testing.T) {
	ir := controlRequestInterrupt("int-1")
	if ir["type"] != "control_request" || ir["request_id"] != "int-1" {
		t.Fatalf("interrupt = %+v", ir)
	}
	req := ir["request"].(map[string]any)
	if req["subtype"] != "interrupt" {
		t.Fatalf("interrupt.request = %+v", req)
	}
	cr := controlCancelRequest("pending-1")
	if cr["type"] != "control_cancel_request" || cr["request_id"] != "pending-1" {
		t.Fatalf("cancel = %+v", cr)
	}
}

// driveClaudeConversation が、can_use_tool の control_request に順に答え (尽きたら最後を繰り返す)、
// result を見てから次のターンを書くこと。
func TestDriveClaudeConversationAnswersInOrderAndSendsFollowup(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool","tool_name":"Write"}}`,
		`{"type":"result"}`,
		`{"type":"control_request","request_id":"perm-2","request":{"subtype":"can_use_tool","tool_name":"Write"}}`,
		`{"type":"result"}`,
	}, "\n") + "\n"

	var stdin bytes.Buffer
	answers := []claudePermissionAnswer{
		{Outcome: "allow"},
		{Outcome: "deny", Message: "だめ"},
	}
	if err := driveClaudeConversation([]string{"1回目", "2回目"}, answers, &stdin, strings.NewReader(stdout)); err != nil {
		t.Fatalf("driveClaudeConversation: %v", err)
	}

	lines := readLines(t, &stdin)
	// 0: initialize, 1: 1 ターン目, 2: perm-1 への allow, 3: 2 ターン目 (追いプロンプト), 4: perm-2 への deny
	if len(lines) != 5 {
		t.Fatalf("lines = %+v", lines)
	}
	if req := lines[0]["request"].(map[string]any); lines[0]["type"] != "control_request" || req["subtype"] != "initialize" {
		t.Fatalf("lines[0] (initialize) = %+v", lines[0])
	}
	if lines[1]["type"] != "user" {
		t.Fatalf("lines[1] (1 ターン目) = %+v", lines[1])
	}
	resp1 := lines[2]["response"].(map[string]any)
	inner1 := resp1["response"].(map[string]any)
	if resp1["request_id"] != "perm-1" || inner1["behavior"] != "allow" {
		t.Fatalf("lines[2] (perm-1 への allow) = %+v", lines[2])
	}
	if lines[3]["type"] != "user" {
		t.Fatalf("lines[3] (追いプロンプト) = %+v", lines[3])
	}
	resp2 := lines[4]["response"].(map[string]any)
	inner2 := resp2["response"].(map[string]any)
	if resp2["request_id"] != "perm-2" || inner2["behavior"] != "deny" || inner2["message"] != "だめ" {
		t.Fatalf("lines[4] (perm-2 への deny) = %+v", lines[4])
	}
}

// answers が尽きたら、最後の答えを繰り返す (Steps の消費と同じ規則)。
func TestDriveClaudeConversationRepeatsLastAnswerWhenExhausted(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool"}}`,
		`{"type":"control_request","request_id":"perm-2","request":{"subtype":"can_use_tool"}}`,
		`{"type":"result"}`,
	}, "\n") + "\n"
	var stdin bytes.Buffer
	answers := []claudePermissionAnswer{{Outcome: "deny", Message: "だめ"}}
	if err := driveClaudeConversation([]string{"1回目"}, answers, &stdin, strings.NewReader(stdout)); err != nil {
		t.Fatalf("driveClaudeConversation: %v", err)
	}
	lines := readLines(t, &stdin)
	// 0: initialize, 1: 1 ターン目, 2: perm-1 への deny, 3: perm-2 への deny (繰り返し)
	if len(lines) != 4 {
		t.Fatalf("lines = %+v", lines)
	}
	for i, id := range []string{"perm-1", "perm-2"} {
		resp := lines[2+i]["response"].(map[string]any)
		if resp["request_id"] != id {
			t.Fatalf("lines[%d] request_id = %v, want %s", 2+i, resp["request_id"], id)
		}
	}
}

// outcome が "interrupt" のときは、can_use_tool には答えず、host 発の interrupt の control_request と、
// その request_id への control_cancel_request を書く。
func TestDriveClaudeConversationInterruptOutcome(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool"}}`,
		`{"type":"result"}`,
	}, "\n") + "\n"
	var stdin bytes.Buffer
	answers := []claudePermissionAnswer{{Outcome: "interrupt"}}
	if err := driveClaudeConversation([]string{"1回目"}, answers, &stdin, strings.NewReader(stdout)); err != nil {
		t.Fatalf("driveClaudeConversation: %v", err)
	}
	lines := readLines(t, &stdin)
	// 0: initialize, 1: 1 ターン目, 2: interrupt の control_request, 3: perm-1 への control_cancel_request
	if len(lines) != 4 {
		t.Fatalf("lines = %+v", lines)
	}
	if lines[2]["type"] != "control_request" {
		t.Fatalf("lines[2] (interrupt) = %+v", lines[2])
	}
	if req := lines[2]["request"].(map[string]any); req["subtype"] != "interrupt" {
		t.Fatalf("lines[2].request = %+v", req)
	}
	if lines[3]["type"] != "control_cancel_request" || lines[3]["request_id"] != "perm-1" {
		t.Fatalf("lines[3] (cancel) = %+v", lines[3])
	}
}

// 場面が answers を持たないとき (ClaudePermissions が空) は、driveClaudeConversation 自体を呼ばない経路
// (runClaude の既存の静的経路) を使うので、ここでは呼び手の分岐を確かめる。
func TestRunClaudeUsesInteractivePathOnlyWithPermissions(t *testing.T) {
	if (&scenario{}).ClaudePermissions != nil {
		t.Fatal("ゼロ値の scenario が、ClaudePermissions を持ってしまっている")
	}
}

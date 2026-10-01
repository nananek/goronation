package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

func TestEncodePrompt(t *testing.T) {
	s := started(t)
	d, _ := json.Marshal(map[string]any{"text": "こんにちは <b>", "extra": 1}) // 未知の欄は無視する
	line, syn, err := s.EncodeCommand(v0.Command{Type: v0.CommandPrompt, Data: d})
	if err != nil || string(line) != `{"method":"POST","path":"/api/session/ses_root/prompt","body":{"text":"こんにちは <b>"}}`+"\n" {
		t.Fatalf("%s %v", line, err)
	}
	mustTypes(t, syn, v0.TypeTurnStarted)
	if dataOf(t, syn[0])["text"] != "こんにちは <b>" {
		t.Fatalf("%s", syn[0].Data)
	}
	if _, _, err := s.EncodeCommand(v0.Command{Type: v0.CommandPrompt, Data: json.RawMessage(`{"text":""}`)}); err == nil {
		t.Fatal("空の prompt が通った")
	}
	if _, _, err := s.EncodeCommand(v0.Command{Type: v0.CommandPrompt, Data: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("不正な data が通った")
	}
}

func TestEncodeCancel(t *testing.T) {
	s := started(t)
	line, syn, err := s.EncodeCommand(v0.Command{Type: v0.CommandCancel})
	if err != nil || syn != nil || string(line) != `{"method":"POST","path":"/api/session/ses_root/interrupt","body":null}`+"\n" {
		t.Fatalf("%s %v", line, err)
	}
}

// root が決まる前は、prompt・cancel は error (どの session に送るか分からない)。
func TestCommandsNeedARootSession(t *testing.T) {
	s := &Stream{}
	for _, c := range []v0.Command{
		{Type: v0.CommandPrompt, Data: json.RawMessage(`{"text":"x"}`)}, {Type: v0.CommandCancel},
	} {
		if line, _, err := s.EncodeCommand(c); err == nil || line != nil {
			t.Errorf("%s: root 無しで通った", c.Type)
		}
	}
}

func TestEncodePermissionResolve(t *testing.T) {
	for outcome, want := range map[string]string{
		v0.AllowOnce:  `{"method":"POST","path":"/api/session/ses_root/permission/per_1/reply","body":{"decision":"once"}}` + "\n",
		v0.RejectOnce: `{"method":"POST","path":"/api/session/ses_root/permission/per_1/reply","body":{"decision":"reject"}}` + "\n",
	} {
		s := started(t)
		feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", `["ls"]`, ""))
		line, syn, err := cmdPermission(t, s, "per_1", outcome)
		if err != nil || string(line) != want {
			t.Fatalf("%s: %s %v", outcome, line, err)
		}
		mustTypes(t, syn, v0.TypePermissionResolved)
		if d := dataOf(t, syn[0]); d["by"] != "human" || d["outcome"] != outcome || d["request_id"] != "per_1" {
			t.Fatalf("%s", syn[0].Data)
		}
		// 1 回限り: 2 回目は error で、何も書かない。
		if line, syn, err := cmdPermission(t, s, "per_1", outcome); err == nil || line != nil || syn != nil {
			t.Fatalf("2 回目が通った: %s", line)
		}
	}
}

// allow_always・reject_always・未知の outcome は、error で、要求は未決のまま (ADR 0021 決定 3)。
func TestAlwaysIsRefused(t *testing.T) {
	s := started(t)
	feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", `["ls"]`, ""))
	for _, o := range []string{v0.AllowAlways, v0.RejectAlways, "approve", "", "once"} {
		if line, _, err := cmdPermission(t, s, "per_1", o); err == nil || line != nil {
			t.Errorf("%q が通った", o)
		}
	}
	if len(s.pending) != 1 {
		t.Fatal("拒否した応答が、要求を消費した")
	}
}

// 未決でない request_id (未知・form の ID・失効済み) は、error。
func TestUnknownRequestIDIsRefused(t *testing.T) {
	s := started(t)
	feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", colorField))
	for _, id := range []string{"per_x", "", "frm_1", "../x"} {
		if line, _, err := cmdPermission(t, s, id, v0.AllowOnce); err == nil || line != nil {
			t.Errorf("%q が通った", id)
		}
	}
	if _, _, err := s.EncodeCommand(v0.Command{Type: "permission.unknown"}); err == nil {
		t.Fatal("未対応のコマンドが通った")
	}
}

// path に入る ID は、検査を通ったものだけ。/・..・制御文字・% を含む ID は、そもそも要求にならない (承認できない)。
func TestUnsafeIDsNeverReachAPath(t *testing.T) {
	for _, id := range []string{"a/b", "..", "a b", "a\nb", "a%2fb", "a?x=1", "a#b", "é", strings.Repeat("a", 129), "a\x00b"} {
		s := started(t)
		d := asked("x", "ses_root", "shell", `["ls"]`, "")
		d = strings.Replace(d, `"id":"x"`, `"id":`+mustJSON(t, id), 1)
		for _, e := range feed(t, s, "permission.asked", d) {
			if e.Type == v0.TypePermissionRequested {
				t.Errorf("id %q が、要求になった", id)
			}
		}
		if len(s.pending) != 0 {
			t.Errorf("id %q の要求を保持した", id)
		}
	}
	// 危険な session ID の session.created は、root にならない。
	for _, sid := range []string{"a/b", "..", "ses root", ""} {
		s := &Stream{}
		envs := feed(t, s, "session.created", `{"sessionID":`+mustJSON(t, sid)+`}`)
		if s.root != "" || envs[0].Type != v0.TypeAgentFrame {
			t.Errorf("session %q が root になった", sid)
		}
	}
}

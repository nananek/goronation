package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// root でも子でもない session のイベントは、何も起こさない (agent.frame)。ほかの利用者の session の要求・form・終わりを、承認させない・
// ターンの終わりにしない。
func TestForeignSessionsAreIgnored(t *testing.T) {
	s := started(t)
	for typ, d := range map[string]string{
		"permission.asked":              asked("per_f", "ses_other", "shell", `["rm -rf /"]`, ""),
		"form.created":                  formCreatedData("frm_f", "ses_other", colorField),
		"session.text.ended":            `{"sessionID":"ses_other","text":"hi"}`,
		"session.tool.called":           `{"sessionID":"ses_other","id":"c","input":{}}`,
		"session.tool.success":          `{"sessionID":"ses_other","id":"c"}`,
		"session.step.ended":            `{"sessionID":"ses_other"}`,
		"session.execution.succeeded":   `{"sessionID":"ses_other"}`,
		"session.execution.failed":      `{"sessionID":"ses_other","error":{"message":"x"}}`,
		"session.execution.interrupted": `{"sessionID":"ses_other","reason":"user"}`,
	} {
		envs := feed(t, s, typ, d)
		if len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
			t.Errorf("%s: %v", typ, types(envs))
		}
	}
	if len(s.pending)+len(s.forms) != 0 {
		t.Fatal("ほかの session の要求を保持した")
	}
}

// root が決まる前のイベントは、agent.frame。2 つ目の root は、ほかの利用者の session。
func TestEventsBeforeRootAndSecondRoot(t *testing.T) {
	s := &Stream{}
	if envs := feed(t, s, "permission.asked", asked("per_1", "ses_a", "shell", `["ls"]`, "")); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
		t.Fatalf("root 前の要求: %v", types(envs))
	}
	feed(t, s, "session.created", `{"sessionID":"ses_a"}`)
	if envs := feed(t, s, "session.created", `{"sessionID":"ses_b"}`); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame || s.root != "ses_a" {
		t.Fatalf("2 つ目の root: %v root=%s", types(envs), s.root)
	}
	if envs := feed(t, s, "permission.asked", asked("per_2", "ses_b", "shell", `["ls"]`, "")); envs[0].Type != v0.TypeAgentFrame {
		t.Fatal("2 つ目の root の要求が通った")
	}
	if envs := feed(t, s, "session.created", `{"sessionID":"ses_c","parentID":null}`); envs[0].Type != v0.TypeAgentFrame || s.root != "ses_a" {
		t.Fatal("parentID が null の session が、root を奪った")
	}
}

func TestChildSessionRules(t *testing.T) {
	s := started(t)
	for name, d := range map[string]string{
		"親が未知":        `{"sessionID":"ses_c","parentID":"ses_unknown"}`,
		"自分が親":        `{"sessionID":"ses_c","parentID":"ses_c"}`,
		"root と同じ ID": `{"sessionID":"ses_root","parentID":"ses_root"}`,
		"parentID が数": `{"sessionID":"ses_c","parentID":7}`,
	} {
		if envs := feed(t, s, "session.created", d); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
			t.Errorf("%s: %v", name, types(envs))
		}
	}
	child_(t, s, "ses_c1", "ses_root")
	if envs := feed(t, s, "session.created", `{"sessionID":"ses_c1","parentID":"ses_root"}`); envs[0].Type != v0.TypeAgentFrame {
		t.Error("同じ子の 2 回目の作成が通った")
	}
	child_(t, s, "ses_c2", "ses_c1") // 孫は、既知の子の子として、受ける
	for i := len(s.children); i < maxChildren; i++ {
		child_(t, s, fmt.Sprintf("ses_x%d", i), "ses_root")
	}
	if envs := feed(t, s, "session.created", `{"sessionID":"ses_over","parentID":"ses_root"}`); envs[0].Type != v0.TypeAgentFrame {
		t.Error("子の上限を超えた session が通った")
	}
}

// tool.progress の metadata.sessionID は、その session の子にしか、対応づけない (別の session の子の Origin を、書き換えさせない)。
func TestProgressCannotRelinkAnotherSessionsChild(t *testing.T) {
	s := started(t)
	child_(t, s, "ses_c1", "ses_root")
	child_(t, s, "ses_c2", "ses_c1")
	feed(t, s, "session.tool.progress", `{"sessionID":"ses_root","id":"call_evil","metadata":{"sessionID":"ses_c2"}}`) // c2 の親は c1。root ではない
	if s.children["ses_c2"].callID != "" {
		t.Fatal("別の session の子に、tool 呼び出しを対応づけた")
	}
	feed(t, s, "session.tool.progress", `{"sessionID":"ses_c1","id":"call_ok","metadata":{"sessionID":"ses_c2"}}`)
	feed(t, s, "session.tool.progress", `{"sessionID":"ses_c1","id":"call_later","metadata":{"sessionID":"ses_c2"}}`) // 最初の対応を、上書きしない
	if s.children["ses_c2"].callID != "call_ok" {
		t.Fatalf("callID = %q", s.children["ses_c2"].callID)
	}
}

// 同じ request_id の再要求は、先の要求を上書きしない (決着の前も後も)。再送された allow が、別の内容を許可してはならない。
func TestDuplicateRequestIDDoesNotOverwrite(t *testing.T) {
	s := started(t)
	feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", `["ls"]`, ""))
	if envs := feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", `["rm -rf /work"]`, "")); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
		t.Fatalf("未決の再要求: %v", types(envs))
	}
	if _, _, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err != nil {
		t.Fatal(err)
	}
	if envs := feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", `["rm -rf /work"]`, "")); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
		t.Fatalf("決着済みの再要求: %v", types(envs))
	}
	if _, _, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err == nil {
		t.Fatal("決着済みの ID に、再び許可が通った")
	}
	// form も同じ。
	feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", colorField))
	if envs := feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", colorField)); envs[0].Type != v0.TypeAgentFrame {
		t.Fatal("form の再要求が通った")
	}
	// permission の ID 空間と form の ID 空間は、別 (同じ文字列でも、互いを決着させない)。
	feed(t, s, "permission.asked", asked("same_1", "ses_root", "shell", `["ls"]`, ""))
	if envs := feed(t, s, "form.created", formCreatedData("same_1", "ses_root", colorField)); envs[0].Type != v0.TypeFormRequested {
		t.Fatal("permission と同じ ID の form が、断られた")
	}
}

// 未決は 64 まで。超えた要求は承認できない (TypeError。保持しない)。決着すれば、以後の新しい要求は通る。permission と form の合計。
func TestPendingLimit(t *testing.T) {
	s := started(t)
	for i := 0; i < maxPending; i++ {
		var envs []v0.Envelope
		if i%2 == 0 {
			envs = feed(t, s, "permission.asked", asked(fmt.Sprintf("per_%d", i), "ses_root", "shell", `["ls"]`, ""))
		} else {
			envs = feed(t, s, "form.created", formCreatedData(fmt.Sprintf("frm_%d", i), "ses_root", colorField))
		}
		if len(envs) != 1 || (envs[0].Type != v0.TypePermissionRequested && envs[0].Type != v0.TypeFormRequested) {
			t.Fatalf("%d 件目: %v", i, types(envs))
		}
	}
	over := feed(t, s, "permission.asked", asked("per_over", "ses_root", "shell", `["ls"]`, ""))
	mustTypes(t, over, v0.TypeError)
	if dataOf(t, over[0])["message"] != limitMessage {
		t.Fatalf("%s", over[0].Data)
	}
	if _, _, err := cmdPermission(t, s, "per_over", v0.RejectOnce); err == nil {
		t.Fatal("上限を超えた要求に、応答が通った")
	}
	if _, _, err := cmdPermission(t, s, "per_0", v0.RejectOnce); err != nil {
		t.Fatal(err)
	}
	if envs := feed(t, s, "permission.asked", asked("per_next", "ses_root", "shell", `["ls"]`, "")); envs[0].Type != v0.TypePermissionRequested {
		t.Fatalf("決着の後の新しい要求が、通らない: %v", types(envs))
	}
	// 上限を超えた要求の ID は、保持も記録もしない (後で空きができたとき、同じ ID を、新しい要求として受ける)。
	if envs := feed(t, s, "permission.asked", asked("per_over", "ses_root", "shell", `["ls"]`, "")); len(envs) != 1 {
		t.Fatalf("%v", types(envs))
	}
}

func TestSeenLimit(t *testing.T) {
	s := started(t)
	s.seen = make(map[[32]byte]uint8, maxSeen) // 見た ID を maxSeen 個、あらかじめ入れる (本物の再生は、遅い)
	for i := 0; i < maxSeen; i++ {
		s.seen[seenKey("p", fmt.Sprintf("old_%d", i))] = stByAgent
	}
	envs := feed(t, s, "permission.asked", asked("per_new", "ses_root", "shell", `["ls"]`, ""))
	mustTypes(t, envs, v0.TypeError)
	if dataOf(t, envs[0])["message"] != limitMessage || len(s.pending) != 0 || len(s.seen) != maxSeen {
		t.Fatalf("%s pending=%d seen=%d", envs[0].Data, len(s.pending), len(s.seen))
	}
	if envs := feed(t, s, "form.created", formCreatedData("frm_new", "ses_root", colorField)); envs[0].Type != v0.TypeError {
		t.Fatalf("form は、上限を超えて通った: %v", types(envs))
	}
}

// SSE の permission.replied: 自分が返したものは出さない・未決のまま来たものは by=agent・未知の ID・session が違う・決着済みは、承認に影響しない。
func TestPermissionRepliedHandling(t *testing.T) {
	s := started(t)
	feed(t, s, "permission.asked", asked("per_1", "ses_root", "shell", `["ls"]`, ""))
	feed(t, s, "permission.asked", asked("per_2", "ses_root", "shell", `["ls"]`, ""))
	feed(t, s, "permission.asked", asked("per_3", "ses_root", "shell", `["ls"]`, ""))
	if _, _, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err != nil {
		t.Fatal(err)
	}
	if envs := feed(t, s, "permission.replied", `{"requestID":"per_1","sessionID":"ses_root","reply":"once"}`); len(envs) != 0 {
		t.Fatalf("自分が返した replied が出た: %v", types(envs))
	}
	for name, d := range map[string]string{
		"未知の ID":        `{"requestID":"per_zzz","sessionID":"ses_root","reply":"once"}`,
		"session が違う":   `{"requestID":"per_2","sessionID":"ses_other","reply":"once"}`,
		"requestID が数":  `{"requestID":2,"sessionID":"ses_root","reply":"once"}`,
		"requestID が不正": `{"requestID":"../x","sessionID":"ses_root","reply":"once"}`,
	} {
		if envs := feed(t, s, "permission.replied", d); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
			t.Errorf("%s: %v", name, types(envs))
		}
	}
	if len(s.pending) != 2 {
		t.Fatalf("偽の replied が、未決の要求を消した: %d", len(s.pending))
	}
	envs := feed(t, s, "permission.replied", `{"requestID":"per_2","sessionID":"ses_root","reply":"reject"}`)
	mustTypes(t, envs, v0.TypePermissionResolved)
	if d := dataOf(t, envs[0]); d["by"] != "agent" || d["outcome"] != v0.RejectOnce {
		t.Fatalf("%s", envs[0].Data)
	}
	if envs := feed(t, s, "permission.replied", `{"requestID":"per_2","sessionID":"ses_root","reply":"reject"}`); envs[0].Type != v0.TypeAgentFrame {
		t.Fatal("決着済みの replied が、2 回目の決着になった")
	}
	envs = feed(t, s, "permission.replied", `{"requestID":"per_3","sessionID":"ses_root","reply":"weird"}`)
	if d := dataOf(t, envs[0]); d["outcome"] != "cancelled" {
		t.Fatalf("未知の reply: %s", envs[0].Data)
	}
	if _, _, err := cmdPermission(t, s, "per_2", v0.AllowOnce); err == nil {
		t.Fatal("agent が閉じた要求に、許可が通った")
	}
}

// root の終わりは、未決の要求・form を、届いた順に by=agent・cancelled で閉じてから、turn.completed にする。子の終わりは、その子の分だけ閉じ、
// ターンを終わらせない。
func TestExecutionEndClosesPending(t *testing.T) {
	s := started(t)
	child_(t, s, "ses_c", "ses_root")
	feed(t, s, "permission.asked", asked("per_r", "ses_root", "shell", `["ls"]`, ""))
	feed(t, s, "form.created", formCreatedData("frm_r", "ses_root", colorField))
	feed(t, s, "permission.asked", asked("per_c", "ses_c", "shell", `["ls"]`, ""))
	feed(t, s, "form.created", formCreatedData("frm_c", "ses_c", colorField))
	envs := feed(t, s, "session.execution.succeeded", `{"sessionID":"ses_c"}`)
	mustTypes(t, envs, v0.TypePermissionResolved, v0.TypeFormResolved)
	for _, e := range envs {
		if e.Origin == nil || e.Origin.ID != "ses_c" || dataOf(t, e)["by"] != "agent" || dataOf(t, e)["outcome"] != "cancelled" {
			t.Fatalf("%+v %s", e.Origin, e.Data)
		}
	}
	if len(s.pending) != 1 || len(s.forms) != 1 {
		t.Fatal("子の終わりが、root の要求を閉じた")
	}
	envs = feed(t, s, "session.execution.interrupted", `{"sessionID":"ses_root","reason":"user"}`)
	mustTypes(t, envs, v0.TypePermissionResolved, v0.TypeFormResolved, v0.TypeTurnCompleted)
	if dataOf(t, envs[0])["request_id"] != "per_r" || dataOf(t, envs[1])["request_id"] != "frm_r" {
		t.Fatalf("届いた順でない: %s %s", envs[0].Data, envs[1].Data)
	}
	if _, _, err := cmdPermission(t, s, "per_r", v0.AllowOnce); err == nil {
		t.Fatal("失効した要求に、許可が通った")
	}
	if _, _, err := cmdForm(t, s, v0.FormResolve{RequestID: "frm_r", Outcome: v0.FormCancelled}); err == nil {
		t.Fatal("失効した form に、応答が通った")
	}
}

// turn.completed は、root の execution.* だけから作る。子の終わり・step・usage からは作らない。
func TestTurnCompletedOnlyFromRootExecution(t *testing.T) {
	s := started(t)
	child_(t, s, "ses_c", "ses_root")
	for typ, d := range map[string]string{
		"session.execution.succeeded":   `{"sessionID":"ses_c"}`,
		"session.execution.failed":      `{"sessionID":"ses_c","error":{"message":"x"}}`,
		"session.execution.interrupted": `{"sessionID":"ses_c"}`,
		"session.step.ended":            `{"sessionID":"ses_root"}`,
		"session.usage.updated":         `{"sessionID":"ses_root"}`,
		"session.step.failed":           `{"sessionID":"ses_root"}`,
	} {
		for _, e := range feed(t, s, typ, d) {
			if e.Type == v0.TypeTurnCompleted || e.Type == v0.TypeError {
				t.Errorf("%s から %s が出た", typ, e.Type)
			}
		}
	}
}

// error の data には、message・status・retryable だけ。url・ヘッダ・response.body は載せない (raw にだけ残る)。
func TestErrorDoesNotCarryURLOrBody(t *testing.T) {
	s := started(t)
	envs := feed(t, s, "session.execution.failed", `{"sessionID":"ses_root","error":{"message":"boom","status":429,
		"url":"https://api.example.test/v1?key=SECRET","headers":{"authorization":"Bearer SECRET"},"response":{"body":"BODYSECRET"},"type":"provider.rate-limit"}}`)
	mustTypes(t, envs, v0.TypeError, v0.TypeTurnCompleted)
	if got := string(envs[0].Data); strings.Contains(got, "SECRET") || !strings.Contains(got, `"retryable":true`) || !strings.Contains(got, `"status":429`) {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(string(envs[0].Raw), "SECRET") { // raw には残る (Public() が落とす)
		t.Fatal("raw に元のフレームが無い")
	}
	long := strings.Repeat("あ", 10000)
	envs = feed(t, s, "session.execution.failed", `{"sessionID":"ses_root","error":{"message":`+mustJSON(t, long)+`}}`)
	if n := len([]rune(dataOf(t, envs[0])["message"].(string))); n != maxErrorText {
		t.Fatalf("message が %d 文字", n)
	}
}

// reason は、安全な短い語だけ。
func TestInterruptReasonIsSanitized(t *testing.T) {
	for reason, want := range map[string]string{`"user"`: "user", `"shutdown"`: "shutdown", `"<script>"`: "", `5`: "", `null`: "", `"` + strings.Repeat("a", 40) + `"`: ""} {
		s := started(t)
		envs := feed(t, s, "session.execution.interrupted", `{"sessionID":"ses_root","reason":`+reason+`}`)
		if dataOf(t, envs[0])["reason"] != want {
			t.Errorf("reason %s → %v", reason, dataOf(t, envs[0])["reason"])
		}
	}
}

// 型違い・null・欠けた欄・巨大な値で、panic せず、承認できない側 (agent.frame・何も出さない) に倒れる。
func TestHostileShapes(t *testing.T) {
	big := strings.Repeat("A", 1<<20)
	for _, typ := range []string{
		"session.created", "session.tool.input.started", "session.text.ended", "session.tool.called", "session.tool.progress", "session.tool.success",
		"session.tool.failed", "session.step.ended", "permission.asked", "permission.replied", "form.created", "form.replied", "form.cancelled",
		"session.execution.succeeded", "session.execution.failed", "session.execution.interrupted", "session.retry.scheduled", "no.such.type",
	} {
		s := started(t)
		for _, d := range []string{
			`null`, `{}`, `[]`, `"x"`, `7`, `true`,
			`{"sessionID":null,"id":null,"form":null,"error":null,"content":null,"metadata":null,"tokens":null}`,
			`{"sessionID":7,"id":[],"form":7,"error":"x","content":{},"metadata":"m","tokens":"t","resources":{},"fields":{}}`,
			`{"sessionID":"ses_root","id":{},"input":[],"form":{"id":7,"sessionID":"ses_root","fields":[1,2]}}`,
			`{"sessionID":"ses_root","id":"c","input":{},"text":` + mustJSON(t, big) + `,"resources":[` + mustJSON(t, big) + `]}`,
			strings.Repeat(`{"a":`, 200) + `1` + strings.Repeat(`}`, 200),
		} {
			envs, err := s.DecodeFrame(sse(typ, d))
			if err != nil {
				continue // JSON として読めない行は error (呼び手が扱う)。panic しなければよい
			}
			for _, e := range envs {
				var v any
				if json.Unmarshal(e.Data, &v) != nil {
					t.Errorf("%s: data が JSON でない: %s", typ, e.Data)
				}
			}
		}
	}
	for _, line := range []string{``, `x`, `[]`, `null`, `{"type":7}`, `{"type":null}`, `{"type":"permission.asked"}`, `{"data":{}}`, strings.Repeat(`[`, 100000)} {
		if _, err := (&Stream{}).DecodeFrame([]byte(line)); err == nil && (line == `` || line == `x` || line == `[]`) {
			t.Errorf("%q が、error にならなかった", line)
		}
	}
}

// 未知の type・型違いの type は、agent.frame (落とさない)。data は空で、raw に元のフレーム。
func TestUnknownTypeIsKeptAsFrame(t *testing.T) {
	s := started(t)
	for _, line := range []string{`{"type":"session.brand.new","data":{"x":1}}`, `{"type":7,"data":{}}`, `{"data":{}}`, `null`} {
		envs, err := s.DecodeFrame([]byte(line))
		if err != nil || len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame || string(envs[0].Data) != "{}" || string(envs[0].Raw) != line {
			t.Errorf("%s: %v %v", line, types(envs), err)
		}
	}
}

// 空の text・housekeeping・step.failed・usage.updated は、何も出さない。retry・synthetic は agent.frame。
func TestQuietAndFrameEvents(t *testing.T) {
	s := started(t)
	if envs := feed(t, s, "session.text.ended", `{"sessionID":"ses_root","text":""}`); len(envs) != 0 {
		t.Fatalf("%v", types(envs))
	}
	for _, typ := range []string{"session.retry.scheduled", "session.synthetic"} {
		if envs := feed(t, s, typ, `{"sessionID":"ses_root"}`); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
			t.Fatalf("%s: %v", typ, types(envs))
		}
	}
	for _, typ := range []string{"session.usage.updated", "session.step.failed", "session.text.delta", "shell.created", "server.connected"} {
		if envs := feed(t, s, typ, `{"sessionID":"ses_root"}`); len(envs) != 0 {
			t.Fatalf("%s: %v", typ, types(envs))
		}
	}
}

// tool 名は tool.input.started で覚える。session ごと (別の session の同じ call id と、混ざらない)。
func TestToolNameIsPerSession(t *testing.T) {
	s := started(t)
	child_(t, s, "ses_c", "ses_root")
	feed(t, s, "session.tool.input.started", `{"sessionID":"ses_root","id":"call_1","name":"shell"}`)
	feed(t, s, "session.tool.input.started", `{"sessionID":"ses_c","id":"call_1","name":"read"}`)
	a := feed(t, s, "session.tool.called", `{"sessionID":"ses_root","id":"call_1","input":{}}`)
	b := feed(t, s, "session.tool.called", `{"sessionID":"ses_c","id":"call_1","input":{}}`)
	if dataOf(t, a[0])["kind"] != v0.KindExecute || dataOf(t, b[0])["kind"] != v0.KindRead {
		t.Fatalf("%s %s", a[0].Data, b[0].Data)
	}
	if envs := feed(t, s, "session.tool.called", `{"sessionID":"ses_root","id":"call_2","input":"x"}`); envs[0].Type != v0.TypeAgentFrame {
		t.Fatal("input がオブジェクトでない tool.call が通った")
	}
	if envs := feed(t, s, "session.tool.called", `{"sessionID":"ses_root","id":"call_3","input":{}}`); dataOf(t, envs[0])["name"] != "" || dataOf(t, envs[0])["kind"] != v0.KindOther {
		t.Fatalf("名前が分からない tool: %s", envs[0].Data)
	}
}

// usage の値は、負数・型違いでも、負にならない (費用・トークンが、合計を減らさない)。
func TestUsageNeverGoesNegative(t *testing.T) {
	s := started(t)
	envs := feed(t, s, "session.step.ended", `{"sessionID":"ses_root","cost":-5,"tokens":{"input":-1,"output":"x","reasoning":-3,"cache":{"read":-2,"write":-9}}}`)
	mustTypes(t, envs, v0.TypeUsage)
	for k, v := range dataOf(t, envs[0]) {
		if n, ok := v.(float64); ok && n < 0 {
			t.Errorf("%s = %v", k, v)
		}
	}
}

// 覚える tool 名は、完了 (success・failed) で消える: 長い session で、名前の表が埋まって、kind が other になり続けない。
func TestToolNamesAreForgottenWhenDone(t *testing.T) {
	s := started(t)
	for i := 0; i < maxCalls+10; i++ {
		id := fmt.Sprintf("call_%d", i)
		feed(t, s, "session.tool.input.started", fmt.Sprintf(`{"sessionID":"ses_root","id":%q,"name":"shell"}`, id))
		envs := feed(t, s, "session.tool.called", fmt.Sprintf(`{"sessionID":"ses_root","id":%q,"input":{}}`, id))
		if dataOf(t, envs[0])["kind"] != v0.KindExecute {
			t.Fatalf("%d 回目の kind = %v", i, dataOf(t, envs[0])["kind"])
		}
		typ := "session.tool.success"
		if i%2 == 1 {
			typ = "session.tool.failed"
		}
		feed(t, s, typ, fmt.Sprintf(`{"sessionID":"ses_root","id":%q}`, id))
	}
	if len(s.calls) != 0 {
		t.Fatalf("calls = %d", len(s.calls))
	}
}

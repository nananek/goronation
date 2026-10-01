package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

func formCreatedData(id, sid, fields string) string {
	return fmt.Sprintf(`{"form":{"id":%q,"sessionID":%q,"title":"T","metadata":{"kind":"question","tool":{"id":"call_q"}},"fields":%s}}`, id, sid, fields)
}

const colorField = `[{"key":"q0","title":"Color","type":"string","custom":true,"options":[{"label":"Red","value":"red"},{"label":"Blue","value":"blue"}]}]`

func TestFormMapping(t *testing.T) {
	s := started(t)
	envs := feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", `[
		{"key":"a","type":"string","options":[{"label":"X","value":"x"}]},
		{"key":"b","type":"string"},
		{"key":"c","type":"multiselect","required":true,"options":[{"label":"L","value":"l"},{"label":"M"}]}]`))
	mustTypes(t, envs, v0.TypeFormRequested)
	var f v0.FormRequested
	if err := json.Unmarshal(envs[0].Data, &f); err != nil {
		t.Fatal(err)
	}
	if f.RequestID != "frm_1" || f.CallID != "call_q" || f.Kind != v0.FormKindQuestion || f.Validate() != nil {
		t.Fatalf("%+v", f)
	}
	if f.Fields[0].Type != v0.FieldSelect || f.Fields[1].Type != v0.FieldText || f.Fields[2].Type != v0.FieldMultiselect || !f.Fields[2].Required {
		t.Fatalf("%+v", f.Fields)
	}
	// 値が無い option は、Label を値にする。値がある option は、書き換えない。
	if f.Fields[2].Options[0].Value != "l" || f.Fields[2].Options[1].Value != "M" {
		t.Fatalf("%+v", f.Fields[2].Options)
	}
}

// kind は、安全な文字だけ。無い・不正なら other。
func TestFormKind(t *testing.T) {
	for kind, want := range map[string]string{`"websearch.provider"`: "websearch.provider", `"a b"`: "other", `"<x>"`: "other", `""`: "other", `7`: "other", `null`: "other",
		fmt.Sprintf("%q", strings.Repeat("a", 65)): "other"} {
		s := started(t)
		d := fmt.Sprintf(`{"form":{"id":"frm_1","sessionID":"ses_root","metadata":{"kind":%s},"fields":%s}}`, kind, colorField)
		envs := feed(t, s, "form.created", d)
		var f v0.FormRequested
		_ = json.Unmarshal(envs[0].Data, &f)
		if f.Kind != want {
			t.Errorf("kind %s → %q, want %q", kind, f.Kind, want)
		}
	}
}

// 形が不正・上限超過の form は、応答できない側 (agent.frame + error。保持しない)。
func TestMalformedFormIsNotAnswerable(t *testing.T) {
	many := make([]string, v0.MaxFormFields+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"key":"k%d","type":"string"}`, i)
	}
	for name, d := range map[string]string{
		"知らない type":                formCreatedData("frm_1", "ses_root", `[{"key":"a","type":"number"}]`),
		"key が無い":                  formCreatedData("frm_1", "ses_root", `[{"type":"string"}]`),
		"key が重複":                  formCreatedData("frm_1", "ses_root", `[{"key":"a","type":"string"},{"key":"a","type":"string"}]`),
		"options が不正":              formCreatedData("frm_1", "ses_root", `[{"key":"a","type":"string","options":"x"}]`),
		"multiselect で options が空": formCreatedData("frm_1", "ses_root", `[{"key":"a","type":"multiselect"}]`),
		"field が 17":               formCreatedData("frm_1", "ses_root", "["+strings.Join(many, ",")+"]"),
		"fields が無い":               formCreatedData("frm_1", "ses_root", `null`),
		"id が不正":                   formCreatedData("frm/1", "ses_root", colorField),
		"form がオブジェクトでない":          `{"form":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := started(t)
			envs := feed(t, s, "form.created", d)
			for _, e := range envs {
				if e.Type == v0.TypeFormRequested {
					t.Fatal("応答できる form になった")
				}
			}
			if len(envs) == 0 || len(s.forms) != 0 {
				t.Fatalf("%v forms=%d", types(envs), len(s.forms))
			}
		})
	}
}

func TestFormResolveIsCheckedAgainstTheRetainedForm(t *testing.T) {
	s := started(t)
	feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", colorField+""))
	bad := map[string]v0.FormResolve{
		"知らない key":           {RequestID: "frm_1", Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"zz": v0.Text("x")}},
		"型が違う (配列)":          {RequestID: "frm_1", Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Many("red")}},
		"未知の outcome":        {RequestID: "frm_1", Outcome: "approved"},
		"request_id が違う":     {RequestID: "frm_2", Outcome: v0.FormCancelled},
		"cancelled に answer": {RequestID: "frm_1", Outcome: v0.FormCancelled, Answer: map[string]v0.FormValue{"q0": v0.Text("red")}},
	}
	for name, r := range bad {
		if line, syn, err := cmdForm(t, s, r); err == nil || line != nil || syn != nil {
			t.Errorf("%s: 通った: %s", name, line)
		}
	}
	if len(s.forms) != 1 { // 失敗した応答は、要求を消費しない
		t.Fatalf("forms = %d", len(s.forms))
	}
	line, syn, err := cmdForm(t, s, v0.FormResolve{RequestID: "frm_1", Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text("自由記述")}})
	if err != nil || string(line) != `{"method":"POST","path":"/api/session/ses_root/form/frm_1/reply","body":{"answer":{"q0":"自由記述"}}}`+"\n" {
		t.Fatalf("%s %v", line, err)
	}
	mustTypes(t, syn, v0.TypeFormResolved)
	if d := dataOf(t, syn[0]); d["by"] != "human" || d["outcome"] != v0.FormAnswered {
		t.Fatalf("%s", syn[0].Data)
	}
	if _, _, err := cmdForm(t, s, v0.FormResolve{RequestID: "frm_1", Outcome: v0.FormCancelled}); err == nil {
		t.Fatal("決着済みの form に、2 回目の応答が通った")
	}
}

func TestFormCancelUsesDelete(t *testing.T) {
	s := started(t)
	feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", colorField))
	line, _, err := cmdForm(t, s, v0.FormResolve{RequestID: "frm_1", Outcome: v0.FormCancelled})
	if err != nil || string(line) != `{"method":"DELETE","path":"/api/session/ses_root/form/frm_1","body":null}`+"\n" {
		t.Fatalf("%s %v", line, err)
	}
}

// multiselect は、配列のまま返る (opencode は配列を受ける: golden question-multiple)。
func TestMultiselectAnswerIsAnArray(t *testing.T) {
	s := started(t)
	feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", `[{"key":"q1","type":"multiselect","options":[{"label":"S","value":"S"},{"label":"L","value":"L"}]}]`))
	line, _, err := cmdForm(t, s, v0.FormResolve{RequestID: "frm_1", Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q1": v0.Many("S", "L")}})
	if err != nil || !strings.Contains(string(line), `"answer":{"q1":["S","L"]}`) {
		t.Fatalf("%s %v", line, err)
	}
}

// 子 session の form は、Origin つき。応答は子の session ID で返る。
func TestChildFormHasOrigin(t *testing.T) {
	s := started(t)
	child_(t, s, "ses_child", "ses_root")
	envs := feed(t, s, "form.created", formCreatedData("frm_c", "ses_child", colorField))
	if len(envs) != 1 || envs[0].Origin == nil || envs[0].Origin.ID != "ses_child" {
		t.Fatalf("%+v", envs)
	}
	line, _, err := cmdForm(t, s, v0.FormResolve{RequestID: "frm_c", Outcome: v0.FormCancelled})
	if err != nil || !strings.Contains(string(line), "/api/session/ses_child/form/frm_c") {
		t.Fatalf("%s %v", line, err)
	}
}

// SSE の form.replied・form.cancelled: 自分が返したものは出さない。未決のまま来たもの (別のクライアント) は by=agent。
func TestFormSettledBySSE(t *testing.T) {
	s := started(t)
	feed(t, s, "form.created", formCreatedData("frm_1", "ses_root", colorField))
	feed(t, s, "form.created", formCreatedData("frm_2", "ses_root", colorField))
	if _, _, err := cmdForm(t, s, v0.FormResolve{RequestID: "frm_1", Outcome: v0.FormCancelled}); err != nil {
		t.Fatal(err)
	}
	if envs := feed(t, s, "form.cancelled", `{"id":"frm_1","sessionID":"ses_root"}`); len(envs) != 0 {
		t.Fatalf("自分が取り消した form の cancelled が、出た: %v", types(envs))
	}
	envs := feed(t, s, "form.replied", `{"id":"frm_2","sessionID":"ses_root","answer":{"q0":"red"}}`)
	mustTypes(t, envs, v0.TypeFormResolved)
	if d := dataOf(t, envs[0]); d["by"] != "agent" || d["outcome"] != v0.FormAnswered || d["request_id"] != "frm_2" {
		t.Fatalf("%s", envs[0].Data)
	}
	if envs := feed(t, s, "form.replied", `{"id":"frm_2","sessionID":"ses_root","answer":{"q0":"red"}}`); len(envs) != 1 || envs[0].Type != v0.TypeAgentFrame {
		t.Fatalf("決着済みの form の 2 回目: %v", types(envs))
	}
}

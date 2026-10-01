package claude

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// form (AskUserQuestion。ADR 0040・0044) と、要約・詳細 (ADR 0041) のテスト。golden は spec/testdata/golden/claude/ask-user-question-*.jsonl (claude 2.1.286)。

type obj = map[string]any

func jsonObj(t *testing.T, b []byte) obj {
	t.Helper()
	var o obj
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// decodeUntilForm は、fixture を、最初の form.requested まで流す。
func decodeUntilForm(t *testing.T, name string) (*Stream, v0.FormRequested, []byte) {
	t.Helper()
	s := Adapter{}.NewStream()
	for _, line := range readLines(t, name) {
		es, err := s.DecodeFrame(line)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Type == v0.TypeFormRequested {
				var f v0.FormRequested
				if err := json.Unmarshal(e.Data, &f); err != nil {
					t.Fatal(err)
				}
				return s.(*Stream), f, line
			}
		}
	}
	t.Fatalf("%s: form.requested が出ない", name)
	return nil, v0.FormRequested{}, nil
}

func formResolveCmd(r v0.FormResolve) v0.Command {
	b, _ := json.Marshal(r)
	return v0.Command{V: v0.Version, Type: v0.CommandFormResolve, Data: b}
}

// responseOf は、control_response の 1 行から、response.response (behavior・updatedInput・message) を読む。
func responseOf(t *testing.T, line []byte) obj {
	t.Helper()
	o := jsonObj(t, line)
	if o["type"] != "control_response" {
		t.Fatalf("control_response でない: %s", line)
	}
	return o["response"].(obj)["response"].(obj)
}

// (a) AskUserQuestion の control_request は、permission.requested でなく form.requested になり、spec/v0 の参照実装 (conformance_test.go の claudeForm) と同じ形。
func TestAskUserQuestionIsAForm(t *testing.T) {
	_, f, frame := decodeUntilForm(t, "ask-user-question-multi")
	cr := jsonObj(t, frame)
	if cr["request"].(obj)["tool_name"] != askUserQuestion || cr["request"].(obj)["requires_user_interaction"] != true {
		t.Fatalf("fixture の形: %s", frame)
	}
	if f.RequestID != cr["request_id"] || f.CallID != "toolu_fake_q1" || f.Kind != v0.FormKindQuestion || f.Title != "" || f.ContentHash != "" {
		t.Fatalf("%+v", f)
	}
	want := []v0.FormField{
		{Key: "q0", Title: "色", Description: "好きな色は?", Type: v0.FieldSelect, Custom: true,
			Options: []v0.FormOption{{Label: "赤", Value: "赤", Description: "暖色"}, {Label: "青", Value: "青", Description: "寒色"}}},
		{Key: "q1", Title: "言語", Description: "使う言語は? (複数可)", Type: v0.FieldMultiselect, Custom: true,
			Options: []v0.FormOption{{Label: "Go", Value: "Go", Description: "静的型"}, {Label: "Python", Value: "Python", Description: "動的型"}, {Label: "Rust", Value: "Rust", Description: "所有権"}}},
	}
	got, _ := json.Marshal(f.Fields)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("fields = %s\nwant    %s", got, exp)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	// 権限の要求としては、出さない。
	for _, e := range decodeAll(t, "ask-user-question-multi") {
		if e.Type == v0.TypePermissionRequested {
			t.Fatalf("AskUserQuestion が permission.requested になった: %s", e.Data)
		}
	}
}

// (b) form.resolve の answered は、保持した input に answers (質問の文 → 回答。複数選択は ", " 連結) を足した allow。golden の tool_use_result.answers と一致する。
func TestFormResolveAnswersMatchGolden(t *testing.T) {
	cases := map[string]v0.FormResolve{
		"single": {Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text("赤")}},
		"multi":  {Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text("青"), "q1": v0.Many("Go", "Rust")}},
		"custom": {Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text("紫 (自由記述)")}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			s, f, frame := decodeUntilForm(t, "ask-user-question-"+name)
			r.RequestID = f.RequestID
			line, es, err := s.EncodeCommand(formResolveCmd(r))
			if err != nil {
				t.Fatal(err)
			}
			if len(es) != 1 || es[0].Type != v0.TypeFormResolved || !es[0].Durable {
				t.Fatalf("合成のイベント: %+v", es)
			}
			if d := data(t, es[0]); d["by"] != "human" || d["outcome"] != v0.FormAnswered || d["request_id"] != f.RequestID || d["answer"] == nil {
				t.Fatalf("form.resolved の data = %v", d)
			}
			resp := responseOf(t, line)
			if resp["behavior"] != "allow" {
				t.Fatalf("response = %v", resp)
			}
			upd := resp["updatedInput"].(obj)
			var wantAnswers any
			for _, l := range readLines(t, "ask-user-question-"+name) {
				if o := jsonObj(t, l); o["type"] == "user" {
					if tr, ok := o["tool_use_result"].(obj); ok {
						wantAnswers = tr["answers"]
					}
				}
			}
			if a, b := mustJSON(upd["answers"]), mustJSON(wantAnswers); a != b {
				t.Fatalf("answers = %s, golden %s", a, b)
			}
			// 保持した input (questions) が、そのまま残る。
			orig := jsonObj(t, mustRaw(jsonObj(t, frame)["request"].(obj)["input"]))
			if mustJSON(upd["questions"]) != mustJSON(orig["questions"]) {
				t.Fatalf("questions が変わった: %s", mustJSON(upd["questions"]))
			}
			// 1 回限り。
			if _, _, err := s.EncodeCommand(formResolveCmd(r)); err == nil {
				t.Error("二重の応答が通った")
			}
		})
	}
}

func mustRaw(v any) []byte  { b, _ := json.Marshal(v); return b }
func mustJSON(v any) string { return string(mustRaw(v)) }

// cancelled は deny (固定の文)。answer は無い。
func TestFormResolveCancelledDenies(t *testing.T) {
	s, f, _ := decodeUntilForm(t, "ask-user-question-deny")
	line, es, err := s.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: f.RequestID, Outcome: v0.FormCancelled}))
	if err != nil {
		t.Fatal(err)
	}
	resp := responseOf(t, line)
	if resp["behavior"] != "deny" || resp["message"] != formDenyMessage || resp["updatedInput"] != nil {
		t.Fatalf("response = %v", resp)
	}
	if d := data(t, es[0]); d["outcome"] != v0.FormCancelled || d["answer"] != nil || d["by"] != "human" {
		t.Fatalf("form.resolved = %v", d)
	}
}

// (e) 拒否: 保持した質問に無いキー・型の違い・cancelled に answer・知らない outcome・未決でない ID・form に permission.resolve・権限に form.resolve は、何も書かず error。未決は残る。
func TestFormResolveRejectsBadInput(t *testing.T) {
	s, f, _ := decodeUntilForm(t, "ask-user-question-multi")
	id := f.RequestID
	for name, r := range map[string]v0.FormResolve{
		"知らないキー":             {RequestID: id, Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q9": v0.Text("x")}},
		"単一の質問に配列":           {RequestID: id, Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Many("赤")}},
		"複数選択に文字列":           {RequestID: id, Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q1": v0.Text("Go")}},
		"長い回答":               {RequestID: id, Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text(strings.Repeat("あ", v0.MaxFormAnswerText+1))}},
		"cancelled に answer": {RequestID: id, Outcome: v0.FormCancelled, Answer: map[string]v0.FormValue{"q0": v0.Text("x")}},
		"知らない outcome":       {RequestID: id, Outcome: "allow_once"},
		"別の ID":              {RequestID: "no-such", Outcome: v0.FormCancelled},
	} {
		if line, es, err := s.EncodeCommand(formResolveCmd(r)); err == nil || line != nil || es != nil {
			t.Errorf("%s: 通った (%s)", name, line)
		}
	}
	// form は、permission.resolve では答えられない (allow すると、回答なしの許可になる)。
	for _, o := range []string{v0.AllowOnce, v0.RejectOnce} {
		if line, _, err := s.EncodeCommand(resolveCmd(id, o)); err == nil || line != nil {
			t.Errorf("form に permission.resolve %s が通った", o)
		}
	}
	// 失敗しても未決のまま残り、正しい応答は通る。
	if _, _, err := s.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: id, Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text("赤")}})); err != nil {
		t.Fatalf("失敗のあとの正しい応答: %v", err)
	}

	// 権限の要求に form.resolve は通らない。
	ps, pe := decodeUntilRequest(t, "permission-interactive-allow")
	pid := data(t, pe)["request_id"].(string)
	if line, _, err := ps.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: pid, Outcome: v0.FormCancelled})); err == nil || line != nil {
		t.Error("権限の要求に form.resolve が通った")
	}
	if _, _, err := ps.EncodeCommand(resolveCmd(pid, v0.AllowOnce)); err != nil {
		t.Errorf("権限は、そのまま答えられる: %v", err)
	}
}

// 撤回 (control_cancel_request)・ターンの終わり (result) は、未決の form を form.resolved (by=agent・cancelled) で閉じる。撤回のあとの応答は error。
func TestFormCancelledByAgent(t *testing.T) {
	s, f, _ := decodeUntilForm(t, "ask-user-question-single")
	es, err := s.DecodeFrame([]byte(`{"type":"control_cancel_request","request_id":"` + f.RequestID + `"}`))
	if err != nil || len(es) != 1 || es[0].Type != v0.TypeFormResolved {
		t.Fatalf("%v %v", err, types(es))
	}
	if d := data(t, es[0]); d["by"] != "agent" || d["outcome"] != v0.FormCancelled || d["request_id"] != f.RequestID {
		t.Fatalf("data = %v", d)
	}
	if _, _, err := s.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: f.RequestID, Outcome: v0.FormCancelled})); err == nil {
		t.Error("撤回のあとの応答が通った")
	}

	// ターンの終わりで閉じる (権限と form が混在しても、届いた順)。
	s2, f2, _ := decodeUntilForm(t, "ask-user-question-single")
	es, err = s2.DecodeFrame([]byte(`{"type":"result","is_error":false,"stop_reason":"end_turn"}`))
	if err != nil || len(es) < 3 || es[0].Type != v0.TypeFormResolved || data(t, es[0])["request_id"] != f2.RequestID {
		t.Fatalf("%v %v", err, types(es))
	}
}

// 形が form にできない AskUserQuestion は、承認も回答もできない: agent.frame + error で知らせ、未決にしない (ID は見たことにする)。
func TestInvalidFormIsAFrameAndAnError(t *testing.T) {
	long := strings.Repeat("a", v0.MaxFormLabelLen+1)
	for name, input := range map[string]string{
		"質問が無い":      `{"questions":[]}`,
		"label が長い":  `{"questions":[{"question":"q","header":"h","options":[{"label":"` + long + `","description":""}]}]}`,
		"選択肢が無い":     `{"questions":[{"question":"q","header":"h","options":[]}]}`,
		"label が重複":  `{"questions":[{"question":"q","header":"h","options":[{"label":"a","description":""},{"label":"a","description":""}]}]}`,
		"質問の文が重複":    `{"questions":[{"question":"q","header":"h","options":[{"label":"a","description":""}]},{"question":"q","header":"h","options":[{"label":"a","description":""}]}]}`,
		"質問の文が空":     `{"questions":[{"question":"","header":"h","options":[{"label":"a","description":""}]}]}`,
		"型が違う":       `{"questions":"x"}`,
		"フィールドが多すぎる": `{"questions":[` + strings.Repeat(`{"question":"q%","header":"h","options":[{"label":"a","description":""}]},`, 17) + `{"question":"last","header":"h","options":[{"label":"a","description":""}]}]}`,
	} {
		if name == "フィールドが多すぎる" { // 質問の文を、全て変える
			var parts []string
			for i := 0; i < 18; i++ {
				parts = append(parts, `{"question":"q`+strings.Repeat("x", i)+`","header":"h","options":[{"label":"a","description":""}]}`)
			}
			input = `{"questions":[` + strings.Join(parts, ",") + `]}`
		}
		s := Adapter{}.NewStream().(*Stream)
		line := `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"t","input":` + input + `}}`
		es, err := s.DecodeFrame([]byte(line))
		if err != nil || len(es) != 2 || es[0].Type != v0.TypeAgentFrame || es[1].Type != v0.TypeError {
			t.Errorf("%s: %v %v", name, err, types(es))
			continue
		}
		if len(s.pending) != 0 {
			t.Errorf("%s: 未決になった", name)
		}
		if _, _, err := s.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: "r1", Outcome: v0.FormCancelled})); err == nil {
			t.Errorf("%s: 応答が通った", name)
		}
		if es, _ := s.DecodeFrame([]byte(line)); len(es) != 1 || es[0].Type != v0.TypeAgentFrame { // 同じ ID の再要求は、新しい要求にならない
			t.Errorf("%s: 再要求: %v", name, types(es))
		}
	}
}

// 表示の文は切るが、回答のキー (質問の文) は、切らない: 長い質問に答えると、claude が出した元の文がキーになる。
func TestLongQuestionKeepsTheOriginalAnswerKey(t *testing.T) {
	q := strings.Repeat("長", v0.MaxFormDescription+50)
	s := Adapter{}.NewStream().(*Stream)
	line := `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"t","input":{"questions":[{"question":"` + q + `","header":"h","options":[{"label":"a","description":""},{"label":"b","description":""}]}]}}}`
	es, err := s.DecodeFrame([]byte(line))
	if err != nil || len(es) != 1 || es[0].Type != v0.TypeFormRequested {
		t.Fatalf("%v %v", err, types(es))
	}
	var f v0.FormRequested
	json.Unmarshal(es[0].Data, &f)
	if n := len([]rune(f.Fields[0].Description)); n != v0.MaxFormDescription || !strings.HasSuffix(f.Fields[0].Description, "…") {
		t.Fatalf("表示の文: %d 文字", n)
	}
	out, _, err := s.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: "r1", Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text("a")}}))
	if err != nil {
		t.Fatal(err)
	}
	answers := responseOf(t, out)["updatedInput"].(obj)["answers"].(obj)
	if answers[q] != "a" || len(answers) != 1 {
		t.Fatalf("answers のキーが、元の質問の文でない")
	}
}

// 空の回答は、answers に入れない。複数選択は ", " で連結する (値に ", " を含んでも、連結のまま)。
func TestFormAnswersEncoding(t *testing.T) {
	s := Adapter{}.NewStream().(*Stream)
	line := `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"t","input":{"questions":[` +
		`{"question":"one","header":"h","options":[{"label":"a","description":""}]},` +
		`{"question":"many","header":"h","multiSelect":true,"options":[{"label":"x, y","description":""},{"label":"z","description":""}]},` +
		`{"question":"skip","header":"h","multiSelect":true,"options":[{"label":"a","description":""}]}],"extra":"keep"}}}`
	if es, err := s.DecodeFrame([]byte(line)); err != nil || es[0].Type != v0.TypeFormRequested {
		t.Fatalf("%v %v", err, types(es))
	}
	out, _, err := s.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: "r1", Outcome: v0.FormAnswered,
		Answer: map[string]v0.FormValue{"q0": v0.Text(""), "q1": v0.Many("x, y", "z"), "q2": v0.Many()}}))
	if err != nil {
		t.Fatal(err)
	}
	upd := responseOf(t, out)["updatedInput"].(obj)
	if mustJSON(upd["answers"]) != `{"many":"x, y, z"}` || upd["extra"] != "keep" {
		t.Fatalf("updatedInput = %v", upd)
	}
}

// (c) 要約と詳細: spec/v0 の参照実装 (claudePermission) と同じ規則。Write・Bash・Edit・Read・WebFetch・知らない tool。
func TestPermissionSummaryAndDetails(t *testing.T) {
	_, e := decodeUntilRequest(t, "permission-interactive-allow")
	var p v0.PermissionRequested
	if err := json.Unmarshal(e.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Summary != "Write: /work/new.txt" || p.Kind != v0.KindEdit || p.DetailsTruncated || p.ContentHash != "" || p.Title != "new.txt" {
		t.Fatalf("%+v", p)
	}
	if len(p.Details) != 2 || p.Details[0] != (v0.Detail{Label: "path", Text: "/work/new.txt", Kind: v0.DetailPath}) ||
		p.Details[1] != (v0.Detail{Label: "content", Text: "new file\n", Kind: v0.DetailText}) {
		t.Fatalf("details = %+v", p.Details)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func decodePermission(t *testing.T, tool, input string) (v0.PermissionRequested, []v0.Envelope) {
	t.Helper()
	s := Adapter{}.NewStream()
	es, err := s.DecodeFrame([]byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"` + tool + `","tool_use_id":"t1","input":` + input + `,"description":"d"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var p v0.PermissionRequested
	if len(es) == 1 && es[0].Type == v0.TypePermissionRequested {
		if err := json.Unmarshal(es[0].Data, &p); err != nil {
			t.Fatal(err)
		}
	}
	return p, es
}

func TestPermissionDetailsPerTool(t *testing.T) {
	long := strings.Repeat("x", 500)
	cases := []struct {
		tool, input, summary string
		details              []v0.Detail
		truncated            bool
	}{
		{"Bash", `{"command":"echo a\necho b # ` + long + `"}`, "", []v0.Detail{{Label: "command", Text: "echo a\necho b # " + long, Kind: v0.DetailCommand}}, false},
		// Bash の、実行に効く他の input (dangerouslyDisableSandbox など) も、詳細に出る (見せていない input を許可させない)。
		{"Bash", `{"command":"ls","dangerouslyDisableSandbox":true,"timeout":5000}`, "Bash: ls",
			[]v0.Detail{{Label: "command", Text: "ls", Kind: v0.DetailCommand}, {Label: "dangerouslyDisableSandbox", Text: "true", Kind: v0.DetailText}, {Label: "timeout", Text: "5000", Kind: v0.DetailText}}, false},
		{"Edit", `{"file_path":"/work/a","old_string":"x","new_string":"y","replace_all":true}`, "Edit: /work/a",
			[]v0.Detail{{Label: "path", Text: "/work/a", Kind: v0.DetailPath}, {Label: "old_string", Text: "x", Kind: v0.DetailText}, {Label: "new_string", Text: "y", Kind: v0.DetailText}, {Label: "replace_all", Text: "true", Kind: v0.DetailText}}, false},
		{"Read", `{"file_path":"/etc/passwd"}`, "Read: /etc/passwd", []v0.Detail{{Label: "path", Text: "/etc/passwd", Kind: v0.DetailPath}}, false},
		{"WebFetch", `{"url":"https://example.com/","prompt":"p"}`, "WebFetch: https://example.com/",
			[]v0.Detail{{Label: "url", Text: "https://example.com/", Kind: v0.DetailURL}, {Label: "prompt", Text: "p", Kind: v0.DetailText}}, false},
		// 知らない tool: 要約は名前・詳細は全てのキーを辞書順に (入れ子は JSON)。
		{"mcp__x__do", `{"b":2,"a":"x","nested":{"k":["v"]}}`, "mcp__x__do",
			[]v0.Detail{{Label: "a", Text: "x", Kind: v0.DetailText}, {Label: "b", Text: "2", Kind: v0.DetailText}, {Label: "nested", Text: `{"k":["v"]}`, Kind: v0.DetailText}}, false},
		// 既知の tool で、値が文字列でなければ、JSON のまま。
		{"Read", `{"file_path":123}`, "Read: 123", []v0.Detail{{Label: "path", Text: "123", Kind: v0.DetailPath}}, false},
	}
	for _, c := range cases {
		p, es := decodePermission(t, c.tool, c.input)
		if p.RequestID != "r1" {
			t.Errorf("%s: %v", c.tool, types(es))
			continue
		}
		if c.summary != "" && p.Summary != c.summary {
			t.Errorf("%s: summary = %q, want %q", c.tool, p.Summary, c.summary)
		}
		if strings.ContainsAny(p.Summary, "\r\n") || len([]rune(p.Summary)) > v0.MaxSummaryLen {
			t.Errorf("%s: summary が 1 行でない・長い: %q", c.tool, p.Summary)
		}
		if mustJSON(p.Details) != mustJSON(c.details) || p.DetailsTruncated != c.truncated {
			t.Errorf("%s: details = %s (truncated=%v), want %s", c.tool, mustJSON(p.Details), p.DetailsTruncated, mustJSON(c.details))
		}
		if p.Validate() != nil {
			t.Errorf("%s: Validate: %v", c.tool, p.Validate())
		}
		if mustJSON(jsonObj(t, p.Input)) != mustJSON(jsonObj(t, []byte(c.input))) {
			t.Errorf("%s: input が書き換わった: %s", c.tool, p.Input)
		}
	}
}

// 上限: 項目の長さ・数・label の長さを超えるときは、切って DetailsTruncated (UI は承認させない)。input は切らない。承認できない形 (tool 名が長い) は agent.frame。
func TestPermissionDetailsLimits(t *testing.T) {
	p, _ := decodePermission(t, "Write", `{"file_path":"/work/a","content":"`+strings.Repeat("あ", v0.MaxDetailTextLen+10)+`"}`)
	if !p.DetailsTruncated || len([]rune(p.Details[1].Text)) != v0.MaxDetailTextLen || p.Validate() != nil {
		t.Fatalf("長い content: truncated=%v", p.DetailsTruncated)
	}
	if !strings.Contains(string(p.Input), strings.Repeat("あ", v0.MaxDetailTextLen+10)) {
		t.Error("input が切られた")
	}
	var kv []string
	for i := 0; i < v0.MaxDetails+4; i++ {
		kv = append(kv, `"k`+strings.Repeat("0", 2)+string(rune('a'+i))+`":1`)
	}
	p, _ = decodePermission(t, "mcp__x__many", `{`+strings.Join(kv, ",")+`}`)
	if !p.DetailsTruncated || len(p.Details) != v0.MaxDetails || p.Validate() != nil {
		t.Fatalf("多すぎるキー: truncated=%v n=%d", p.DetailsTruncated, len(p.Details))
	}
	p, _ = decodePermission(t, "mcp__x__long", `{"`+strings.Repeat("k", v0.MaxDetailLabelLen+1)+`":1}`)
	if !p.DetailsTruncated || len([]rune(p.Details[0].Label)) != v0.MaxDetailLabelLen || p.Validate() != nil {
		t.Fatalf("長いキー: truncated=%v", p.DetailsTruncated)
	}
	if _, es := decodePermission(t, strings.Repeat("T", v0.MaxToolNameLen+1), `{}`); len(es) != 1 || es[0].Type != v0.TypeAgentFrame {
		t.Fatalf("長い tool 名: %v", types(es))
	}
}

// 権限と form は、同じ request_id の名前空間・同じ上限を持つ (form も、未決の上限に数える)。
func TestFormsShareThePendingLimit(t *testing.T) {
	s := Adapter{}.NewStream().(*Stream)
	form := func(id string) string {
		return `{"type":"control_request","request_id":"` + id + `","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"t","input":{"questions":[{"question":"q","header":"h","options":[{"label":"a","description":""}]}]}}}`
	}
	perm := func(id string) string {
		return `{"type":"control_request","request_id":"` + id + `","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"t","input":{"command":"ls"}}}`
	}
	for i := 0; i < maxPendingRequests; i++ {
		line := form("f" + strconv.Itoa(i))
		if i%2 == 1 {
			line = perm("p" + strconv.Itoa(i))
		}
		if es, err := s.DecodeFrame([]byte(line)); err != nil || (es[0].Type != v0.TypeFormRequested && es[0].Type != v0.TypePermissionRequested) {
			t.Fatalf("%d: %v %v", i, err, types(es))
		}
	}
	if es, _ := s.DecodeFrame([]byte(form("over"))); len(es) != 1 || es[0].Type != v0.TypeError {
		t.Fatalf("上限を超えた form: %v", types(es))
	}
	// 同じ ID の再要求 (権限の ID で form を要求) は、新しい要求にならない。
	if es, _ := s.DecodeFrame([]byte(form("p1"))); len(es) != 1 || es[0].Type != v0.TypeAgentFrame {
		t.Fatalf("同じ ID の再要求: %v", types(es))
	}
}

// claude が input に先回りして answers を入れていても、応答の answers は、利用者の回答だけ (上書きする)。
func TestFormAnswersOverrideInputAnswers(t *testing.T) {
	s := Adapter{}.NewStream().(*Stream)
	line := `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"t","input":{"questions":[` +
		`{"question":"one","header":"h","options":[{"label":"a","description":""},{"label":"b","description":""}]}],"answers":{"one":"b","forged":"x"}}}}`
	if es, err := s.DecodeFrame([]byte(line)); err != nil || es[0].Type != v0.TypeFormRequested {
		t.Fatalf("%v %v", err, types(es))
	}
	out, _, err := s.EncodeCommand(formResolveCmd(v0.FormResolve{RequestID: "r1", Outcome: v0.FormAnswered, Answer: map[string]v0.FormValue{"q0": v0.Text("a")}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(responseOf(t, out)["updatedInput"].(obj)["answers"]); got != `{"one":"a"}` {
		t.Fatalf("answers = %s", got)
	}
}

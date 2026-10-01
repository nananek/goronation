package v0

// 適合テスト (ADR 0040〜0044): claude と opencode の実フレーム (spec/testdata/golden) を、この package の語彙に写せることを確かめる。
// 写し方 (参照実装) は、このファイルの中にあり、テストだけが使う。アダプタ (agent/claude・agent/opencode。PR⑤b・PR⑤) は、同じ規則で
// 書き、同じ fixtures で、同じ結果になることを、それぞれのテストで確かめる。規則の正は ADR (表) と、このファイルの期待値。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type obj = map[string]any

func ndjson(t *testing.T, path ...string) []obj {
	t.Helper()
	p := filepath.Join(append([]string{"..", "testdata", "golden"}, path...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []obj
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var o obj
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, o)
	}
	return out
}

func get(o obj, path ...string) any {
	var cur any = o
	for _, k := range path {
		m, ok := cur.(obj)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func gs(o obj, path ...string) string { s, _ := get(o, path...).(string); return s }

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// ---- claude の AskUserQuestion → form (ADR 0040・0044) ----

// claudeForm は、AskUserQuestion の can_use_tool (control_request) を、FormRequested にする。質問の文は、回答のキー (claude の answers は
// 質問の文がキー) として、アダプタが保持する (戻り値 texts[key])。
func claudeForm(t *testing.T, f obj) (FormRequested, map[string]string) {
	t.Helper()
	qs, _ := get(f, "request", "input", "questions").([]any)
	form := FormRequested{RequestID: gs(f, "request_id"), CallID: gs(f, "request", "tool_use_id"), Kind: FormKindQuestion}
	texts := map[string]string{}
	for i, q := range qs {
		qo := q.(obj)
		key := "q" + string(rune('0'+i))
		fd := FormField{Key: key, Title: qo["header"].(string), Description: qo["question"].(string), Type: FieldSelect, Custom: true}
		if m, _ := qo["multiSelect"].(bool); m {
			fd.Type = FieldMultiselect
		}
		for _, o := range qo["options"].([]any) {
			oo := o.(obj)
			fd.Options = append(fd.Options, FormOption{Label: oo["label"].(string), Value: oo["label"].(string), Description: oo["description"].(string)})
		}
		form.Fields = append(form.Fields, fd)
		texts[key] = qo["question"].(string)
	}
	return form, texts
}

// claudeAnswers は、FormResolve を、claude の updatedInput.answers (質問の文 → 回答の文字列。複数選択は ", " で連結) にする。
func claudeAnswers(texts map[string]string, r FormResolve) map[string]any {
	out := obj{}
	for k, v := range r.Answer {
		out[texts[k]] = strings.Join(v.Values, ", ")
	}
	return out
}

func findControlRequest(t *testing.T, frames []obj) obj {
	t.Helper()
	for _, f := range frames {
		if f["type"] == "control_request" && gs(f, "request", "subtype") == "can_use_tool" {
			return f
		}
	}
	t.Fatal("can_use_tool の control_request が無い")
	return nil
}

func toolUseResult(t *testing.T, frames []obj) obj {
	t.Helper()
	for _, f := range frames {
		if f["type"] == "user" {
			if r, ok := f["tool_use_result"].(obj); ok {
				return r
			}
		}
	}
	t.Fatal("tool_use_result が無い")
	return nil
}

func TestClaudeAskUserQuestionIsAForm(t *testing.T) {
	scenes := map[string]struct {
		answer FormResolve
		want   obj // golden の tool_use_result.answers
	}{
		"single": {FormResolve{Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("赤")}}, obj{"好きな色は?": "赤"}},
		"multi": {FormResolve{Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("青"), "q1": Many("Go", "Rust")}},
			obj{"好きな色は?": "青", "使う言語は? (複数可)": "Go, Rust"}},
		"custom":     {FormResolve{Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("紫 (自由記述)")}}, obj{"好きな色は?": "紫 (自由記述)"}},
		"unanswered": {FormResolve{Outcome: FormCancelled}, obj{}},
	}
	for name, sc := range scenes {
		t.Run(name, func(t *testing.T) {
			frames := ndjson(t, "claude", "ask-user-question-"+name+".jsonl")
			cr := findControlRequest(t, frames)
			// claude の要求は、対話が要る (requires_user_interaction)・表示名つき。permission.requested ではなく form.requested にする根拠。
			if cr["request"].(obj)["requires_user_interaction"] != true || gs(cr, "request", "tool_name") != "AskUserQuestion" {
				t.Fatalf("AskUserQuestion の要求の形: %v", cr["request"])
			}
			form, texts := claudeForm(t, cr)
			if err := form.Validate(); err != nil {
				t.Fatal(err)
			}
			if form.Hash() == "" {
				t.Fatal("hash")
			}
			sc.answer.RequestID = form.RequestID
			if err := sc.answer.Validate(form); err != nil {
				t.Fatal(err)
			}
			if sc.answer.Outcome == FormAnswered {
				// 保持した質問の文に対して作った answers が、実際に claude が受け付けた (golden の) answers と一致する。
				got := claudeAnswers(texts, sc.answer)
				if string(raw(got)) != string(raw(sc.want)) {
					t.Fatalf("answers = %s, want %s", raw(got), raw(sc.want))
				}
				if g := toolUseResult(t, frames)["answers"]; string(raw(g)) != string(raw(sc.want)) {
					t.Fatalf("golden の answers = %s, want %s", raw(g), raw(sc.want))
				}
			} else if g, _ := toolUseResult(t, frames)["answers"].(obj); len(g) != 0 {
				t.Fatalf("answers なしの allow で、answers が空でない: %v", g)
			}
		})
	}
}

// 根本原因 (ADR 0044): いまの応答 (updatedInput に、要求の input だけ) は、答えを運ばず、claude は「回答なし」で tool を終える。
func TestClaudeAskUserQuestionWithoutAnswersIsTheCurrentBug(t *testing.T) {
	frames := ndjson(t, "claude", "ask-user-question-unanswered.jsonl")
	var content string
	for _, f := range frames {
		if f["type"] != "user" {
			continue
		}
		if m, ok := get(f, "message").(obj); ok {
			if cs, ok := m["content"].([]any); ok && len(cs) > 0 {
				content, _ = cs[0].(obj)["content"].(string)
			}
		}
	}
	if content != "The user did not answer the questions." {
		t.Fatalf("tool_result = %q", content)
	}
}

func TestClaudeAskUserQuestionDenyIsACancel(t *testing.T) {
	frames := ndjson(t, "claude", "ask-user-question-deny.jsonl")
	var isErr bool
	for _, f := range frames {
		if m, ok := get(f, "message").(obj); ok && f["type"] == "user" {
			if cs, ok := m["content"].([]any); ok && len(cs) > 0 {
				isErr, _ = cs[0].(obj)["is_error"].(bool)
			}
		}
	}
	if !isErr {
		t.Fatal("deny の tool_result が is_error でない")
	}
}

// ---- opencode の form → form (ADR 0040) ----

func opencodeForm(t *testing.T, e obj) FormRequested {
	t.Helper()
	fm := get(e, "data", "form").(obj)
	form := FormRequested{RequestID: gs(fm, "id"), CallID: gs(fm, "metadata", "tool", "id"), Kind: gs(fm, "metadata", "kind"), Title: gs(fm, "title")}
	for _, f := range fm["fields"].([]any) {
		fo := f.(obj)
		fd := FormField{Key: gs(fo, "key"), Title: gs(fo, "title"), Description: gs(fo, "description")}
		fd.Custom, _ = fo["custom"].(bool)
		fd.Required, _ = fo["required"].(bool)
		opts, _ := fo["options"].([]any)
		switch fo["type"] {
		case "multiselect":
			fd.Type = FieldMultiselect
		case "string":
			fd.Type = FieldSelect
			if len(opts) == 0 {
				fd.Type = FieldText
			}
		default:
			t.Fatalf("未知の type %v", fo["type"])
		}
		for _, o := range opts {
			oo := o.(obj)
			fd.Options = append(fd.Options, FormOption{Label: gs(oo, "label"), Value: gs(oo, "value"), Description: gs(oo, "description")})
		}
		form.Fields = append(form.Fields, fd)
	}
	return form
}

func firstEvent(es []obj, typ string) obj {
	for _, e := range es {
		if e["type"] == typ {
			return e
		}
	}
	return nil
}

func TestOpencodeFormsAreForms(t *testing.T) {
	cases := []struct {
		scene, kind string
		reply       FormResolve
	}{
		{"question-single", FormKindQuestion, FormResolve{Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red")}}},
		{"question-multiple", FormKindQuestion, FormResolve{Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Blue"), "q1": Many("S", "L")}}},
		{"question-custom", FormKindQuestion, FormResolve{Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Green (my own answer)")}}},
		{"question-cancel", FormKindQuestion, FormResolve{Outcome: FormCancelled}},
		{"websearch-form", "websearch.provider", FormResolve{Outcome: FormCancelled}},
	}
	for _, c := range cases {
		t.Run(c.scene, func(t *testing.T) {
			ev := ndjson(t, "opencode-serve", c.scene, "events.ndjson")
			form := opencodeForm(t, firstEvent(ev, "form.created"))
			if form.Kind != c.kind {
				t.Fatalf("kind = %q, want %q", form.Kind, c.kind)
			}
			if err := form.Validate(); err != nil {
				t.Fatal(err)
			}
			c.reply.RequestID = form.RequestID
			if err := c.reply.Validate(form); err != nil {
				t.Fatal(err)
			}
			// 実際に送った返答 (requests.ndjson の POST …/form/{id}/reply の body) と、この語彙の回答が、同じ中身になる。
			for _, r := range ndjson(t, "opencode-serve", c.scene, "requests.ndjson") {
				if r["method"] == "POST" && strings.HasSuffix(gs(r, "path"), "/reply") && strings.Contains(gs(r, "path"), "/form/") {
					want := raw(get(r, "body", "answer"))
					got := raw(c.reply.Answer)
					if string(want) != string(got) {
						t.Fatalf("answer = %s, want %s", got, want)
					}
				}
			}
		})
	}
}

// 汎用の form: websearch の provider の選択は、質問ではなく、設定の選択。語彙は、kind で挙動を変えず、フィールドの一覧として表す。
func TestOpencodeWebsearchFormIsNotAQuestion(t *testing.T) {
	ev := ndjson(t, "opencode-serve", "websearch-form", "events.ndjson")
	form := opencodeForm(t, firstEvent(ev, "form.created"))
	if form.Kind == FormKindQuestion || len(form.Fields) != 1 || !form.Fields[0].Required || form.Fields[0].Custom {
		t.Fatalf("%+v", form)
	}
	// options だけ (custom でない) のフィールドに、options に無い値は通らない。
	bad := FormResolve{RequestID: form.RequestID, Outcome: FormAnswered, Answer: map[string]FormValue{"choice": Text("exa-but-evil")}}
	if err := bad.Validate(form); err == nil {
		t.Fatal("options に無い値が通った")
	}
	ok := FormResolve{RequestID: form.RequestID, Outcome: FormAnswered, Answer: map[string]FormValue{"choice": Text("allow")}}
	if err := ok.Validate(form); err != nil {
		t.Fatal(err)
	}
}

// ---- 権限の要求 → 要約 + 詳細 (ADR 0041) ----

func toolKindOf(name string) string {
	switch name {
	case "Bash", "shell":
		return KindExecute
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "edit", "write":
		return KindEdit
	case "Read", "read", "external_directory":
		return KindRead
	case "Glob", "Grep", "glob", "grep", "websearch", "WebSearch":
		return KindSearch
	case "WebFetch", "webfetch":
		return KindFetch
	}
	return KindOther
}

func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64, bool:
		return string(raw(x)), true
	}
	return "", false
}

// claudePermission は、claude の can_use_tool (Bash・Write・Edit・Read・WebFetch と、その他) を、PermissionRequested にする。
func claudePermission(t *testing.T, f obj) PermissionRequested {
	t.Helper()
	in, _ := get(f, "request", "input").(obj)
	name := gs(f, "request", "tool_name")
	p := PermissionRequested{RequestID: gs(f, "request_id"), CallID: gs(f, "request", "tool_use_id"), ToolName: name,
		Kind: toolKindOf(name), Input: raw(in), Title: gs(f, "request", "description")}
	var details []Detail
	trunc := false
	add := func(label, text, kind string) {
		s, cut := ClampText(text, MaxDetailTextLen)
		trunc = trunc || cut
		details = append(details, Detail{Label: label, Text: s, Kind: kind})
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch name {
	case "Bash":
		p.Summary = SummaryLine("Bash: "+str("command"), MaxSummaryLen)
		add("command", str("command"), DetailCommand)
	case "Write":
		p.Summary = SummaryLine("Write: "+str("file_path"), MaxSummaryLen)
		add("path", str("file_path"), DetailPath)
		add("content", str("content"), DetailText)
	case "Edit":
		p.Summary = SummaryLine("Edit: "+str("file_path"), MaxSummaryLen)
		add("path", str("file_path"), DetailPath)
		add("old_string", str("old_string"), DetailText)
		add("new_string", str("new_string"), DetailText)
	case "Read":
		p.Summary = SummaryLine("Read: "+str("file_path"), MaxSummaryLen)
		add("path", str("file_path"), DetailPath)
	case "WebFetch":
		p.Summary = SummaryLine("WebFetch: "+str("url"), MaxSummaryLen)
		add("url", str("url"), DetailURL)
		add("prompt", str("prompt"), DetailText)
	default: // 知らない tool: 名前だけを要約にし、input の最上位のスカラーを、キーの辞書順に詳細にする (落とさない。入れ子は JSON)。
		p.Summary = SummaryLine(name, MaxSummaryLen)
		keys := make([]string, 0, len(in))
		for k := range in {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if s, ok := scalar(in[k]); ok {
				add(k, s, DetailText)
			} else {
				add(k, string(raw(in[k])), DetailText)
			}
		}
	}
	p.Details, p.DetailsTruncated = details, trunc
	return p
}

// opencodePermission は、opencode の permission.asked を、PermissionRequested にする。input は、承認する対象 (action・resources・metadata)。
// id・sessionID・source は、経路の情報なので input に入れない (request_id・call_id に写す)。save は always の保存の対象で、v0 は always を出さない。
func opencodePermission(t *testing.T, e obj) PermissionRequested {
	t.Helper()
	d := get(e, "data").(obj)
	action := gs(d, "action")
	var resources []string
	for _, r := range d["resources"].([]any) {
		resources = append(resources, r.(string))
	}
	in := obj{"action": action, "resources": resources}
	if m, ok := d["metadata"].(obj); ok {
		in["metadata"] = m
	}
	p := PermissionRequested{RequestID: gs(d, "id"), CallID: gs(d, "source", "id"), ToolName: action, Kind: toolKindOf(action), Input: raw(in)}
	sep := ", "
	label, dk := "target", DetailText
	switch action {
	case "shell":
		sep, label, dk = " ; ", "command", DetailCommand // 部分コマンドの一覧。1 つずつ、見せる (ADR 0021 決定 4)
	case "edit", "write", "read", "external_directory":
		label, dk = "path", DetailPath
	case "glob", "grep":
		label = "pattern"
	case "webfetch":
		label, dk = "url", DetailURL
	case "websearch":
		label = "query"
	}
	p.Summary = SummaryLine(action+": "+strings.Join(resources, sep), MaxSummaryLen)
	trunc := false
	add := func(l, text, k string) {
		s, cut := ClampText(text, MaxDetailTextLen)
		trunc = trunc || cut
		p.Details = append(p.Details, Detail{Label: l, Text: s, Kind: k})
	}
	for _, r := range resources {
		add(label, r, dk)
	}
	if md, ok := d["metadata"].(obj); ok {
		if files, ok := md["files"].([]any); ok { // edit・write: 変更の差分 (patch) も、見せる
			for _, f := range files {
				fo := f.(obj)
				add("patch: "+gs(fo, "file"), gs(fo, "patch"), DetailText)
			}
		}
		for _, k := range []string{"url", "query", "root", "format"} {
			if s, ok := md[k].(string); ok && !slices.Contains(resources, s) {
				add(k, s, DetailText)
			}
		}
	}
	p.DetailsTruncated = trunc
	return p
}

func TestClaudePermissionMapsToSummaryAndDetails(t *testing.T) {
	frames := ndjson(t, "claude", "permission-interactive-allow.jsonl")
	p := claudePermission(t, findControlRequest(t, frames))
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Summary != "Write: /work/new.txt" || p.Kind != KindEdit || p.DetailsTruncated {
		t.Fatalf("%+v", p)
	}
	if len(p.Details) != 2 || p.Details[0] != (Detail{Label: "path", Text: "/work/new.txt", Kind: DetailPath}) ||
		p.Details[1] != (Detail{Label: "content", Text: "new file\n", Kind: DetailText}) {
		t.Fatalf("details = %+v", p.Details)
	}
	// input は書き換えない。
	var in obj
	if err := json.Unmarshal(p.Input, &in); err != nil || in["file_path"] != "/work/new.txt" || in["content"] != "new file\n" {
		t.Fatalf("input = %s", p.Input)
	}
}

// 要約は、エージェントが書いた値を含むので、1 行にして、長さを切る。切ったことを、詳細の欠けにしない (詳細は、別に、全体を持つ)。
func TestSummaryIsOneLineAndDetailsKeepTheWholeValue(t *testing.T) {
	cmd := "echo a\necho b # " + strings.Repeat("x", 500)
	f := obj{"request_id": "r", "request": obj{"tool_name": "Bash", "tool_use_id": "c", "input": obj{"command": cmd}}}
	p := claudePermission(t, f)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(p.Summary, "\r\n") || len([]rune(p.Summary)) > MaxSummaryLen || !strings.HasSuffix(p.Summary, "…") {
		t.Fatalf("summary = %q", p.Summary)
	}
	if p.Details[0].Text != cmd || p.DetailsTruncated {
		t.Fatalf("詳細が、コマンド全体でない: %d 文字・truncated=%v", len([]rune(p.Details[0].Text)), p.DetailsTruncated)
	}
	// 詳細の上限を超えるときは、切って、DetailsTruncated を立てる (UI は、承認させない)。
	big := strings.Repeat("あ", MaxDetailTextLen+10)
	q := claudePermission(t, obj{"request_id": "r", "request": obj{"tool_name": "Write", "tool_use_id": "c", "input": obj{"file_path": "/work/a", "content": big}}})
	if err := q.Validate(); err != nil || !q.DetailsTruncated || len([]rune(q.Details[1].Text)) != MaxDetailTextLen {
		t.Fatalf("err=%v truncated=%v", err, q.DetailsTruncated)
	}
}

// 知らない tool も、落とさず、input の全てを詳細にする (承認する対象が、見えないままにならない)。
func TestUnknownClaudeToolKeepsAllInputInDetails(t *testing.T) {
	f := obj{"request_id": "r", "request": obj{"tool_name": "mcp__x__do", "tool_use_id": "c",
		"input": obj{"b": float64(2), "a": "x", "nested": obj{"k": []any{"v"}}}}}
	p := claudePermission(t, f)
	if p.Summary != "mcp__x__do" || p.Kind != KindOther || len(p.Details) != 3 {
		t.Fatalf("%+v", p)
	}
	if p.Details[0].Label != "a" || p.Details[1].Label != "b" || p.Details[2].Label != "nested" || p.Details[2].Text != `{"k":["v"]}` {
		t.Fatalf("details = %+v", p.Details)
	}
}

func TestOpencodePermissionsMapToSummaryAndDetails(t *testing.T) {
	want := map[string]struct {
		summary, kind string
		details       []Detail
	}{
		"shell":              {"shell: echo goro-hi", KindExecute, []Detail{{"command", "echo goro-hi", DetailCommand}}},
		"edit":               {"edit: notes.txt", KindEdit, nil}, // details[0] は path。patch は後ろに続く
		"read":               {"read: notes.txt", KindRead, []Detail{{"path", "notes.txt", DetailPath}}},
		"external_directory": {"external_directory: /etc/*", KindRead, []Detail{{"path", "/etc/*", DetailPath}}},
		"glob":               {"glob: *.txt", KindSearch, []Detail{{"pattern", "*.txt", DetailText}, {"root", ".", DetailText}}},
		"grep":               {"grep: goodbye", KindSearch, []Detail{{"pattern", "goodbye", DetailText}, {"root", ".", DetailText}}},
		"webfetch":           {"webfetch: http://fake-provider.test/page", KindFetch, []Detail{{"url", "http://fake-provider.test/page", DetailURL}, {"format", "text", DetailText}}},
		"skill":              {"skill: opencode", KindOther, []Detail{{"target", "opencode", DetailText}}},
		"subagent":           {"subagent: general", KindOther, []Detail{{"target", "general", DetailText}}},
		"websearch":          {"websearch: goronation", KindSearch, []Detail{{"query", "goronation", DetailText}}},
		"question":           {"question: *", KindOther, []Detail{{"target", "*", DetailText}}},
	}
	seen := map[string]bool{}
	hashes := map[string]string{}
	for _, scene := range []string{"actions", "tool-call-shell-allow", "subagent", "websearch-form", "question-cancel"} {
		for _, e := range ndjson(t, "opencode-serve", scene, "events.ndjson") {
			if e["type"] != "permission.asked" {
				continue
			}
			p := opencodePermission(t, e)
			if err := p.Validate(); err != nil {
				t.Fatalf("%s/%s: %v", scene, p.ToolName, err)
			}
			w, ok := want[p.ToolName]
			if !ok || seen[p.ToolName] {
				continue
			}
			seen[p.ToolName] = true
			if p.Summary != w.summary || p.Kind != w.kind {
				t.Errorf("%s: summary=%q kind=%q, want %q %q", p.ToolName, p.Summary, p.Kind, w.summary, w.kind)
			}
			if w.details != nil && !slices.Equal(p.Details, w.details) {
				t.Errorf("%s: details = %+v, want %+v", p.ToolName, p.Details, w.details)
			}
			if p.ToolName == "edit" && (p.Details[0].Kind != DetailPath || len(p.Details) < 2 || !strings.HasPrefix(p.Details[1].Label, "patch: ") ||
				!strings.Contains(p.Details[1].Text, "+hello")) {
				t.Errorf("edit の詳細に、path と patch が無い: %+v", p.Details)
			}
			var in obj
			if err := json.Unmarshal(p.Input, &in); err != nil || in["action"] != p.ToolName {
				t.Errorf("input = %s", p.Input)
			}
			if h := p.Hash(); h == "" || hashes[h] != "" {
				t.Errorf("%s: hash が空か、別の要求と同じ: %q", p.ToolName, h)
			} else {
				hashes[h] = p.ToolName
			}
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("action %q の permission.asked が、fixtures に無い", k)
		}
	}
}

// shell は、部分コマンドが複数のとき、1 つずつ見せる (1 つでも見えないまま、許可させない。ADR 0021 決定 4)。
func TestOpencodeShellShowsEveryPartialCommand(t *testing.T) {
	e := obj{"type": "permission.asked", "data": obj{"action": "shell", "id": "per_1", "resources": []any{"git status", "rm -rf /tmp/x"},
		"source": obj{"id": "call_1"}}}
	p := opencodePermission(t, e)
	if p.Summary != "shell: git status ; rm -rf /tmp/x" || len(p.Details) != 2 || p.Details[1].Text != "rm -rf /tmp/x" {
		t.Fatalf("%+v", p)
	}
}

// ---- サブエージェントの帰属 (ADR 0044) ----

func TestOpencodeSubagentSessionHasAnOrigin(t *testing.T) {
	ev := ndjson(t, "opencode-serve", "subagent", "events.ndjson")
	var parent string
	for _, e := range ev {
		if e["type"] == "session.created" && get(e, "data", "parentID") == nil {
			parent = gs(e, "data", "sessionID")
		}
	}
	if parent == "" {
		t.Fatal("親の session が無い")
	}
	var origins []Origin
	for _, e := range ev {
		if e["type"] == "session.created" && gs(e, "data", "parentID") == parent {
			origins = append(origins, Origin{ID: gs(e, "data", "sessionID"), Parent: "call_sub"})
		}
	}
	if len(origins) != 1 || origins[0].ID == "" || origins[0].ID == parent {
		t.Fatalf("origins = %+v", origins)
	}
	env := Envelope{V: Version, Type: TypeMessageText, Origin: &origins[0], Data: json.RawMessage(`{}`)}
	b, _ := json.Marshal(env.Public())
	if !strings.Contains(string(b), `"origin":{"id":"`) {
		t.Fatalf("Public() が origin を落とした: %s", b)
	}
	// メインのエージェント自身の出力には、origin が付かない。
	b, _ = json.Marshal(Envelope{V: Version, Type: TypeMessageText, Data: json.RawMessage(`{}`)}.Public())
	if strings.Contains(string(b), "origin") {
		t.Fatalf("origin が付いた: %s", b)
	}
}

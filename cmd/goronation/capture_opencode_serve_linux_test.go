//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/framenorm"
	"github.com/nananek/goronation/tools/fakeproviders/openai"
)

// opencode 2.x の serve (HTTP + SSE) の golden fixtures の採取 (M2 PR④)。
//
// 実物の opencode を、本番と同じ経路 (cageSpec → goronation init --relay-control --relay-token-env --landlock-connect --relay-version-prefix → landlock-exec → opencode。
// 起動の証明・pidfd と待ち受けの確認・ヘッダの許可リスト・トークンは init だけが持つ) で動かし (startRealCage。TestRealOpencodeInCage と同じ道具)、偽の provider に決まった応答を
// 返させて、ホストが送る要求と SSE の全イベントを、場面ごとに記録する。
//
// GORO_REAL_OPENCODE=<opencode 2.0.x の実行ファイル> と GORO_CAPTURE_OPENCODE_SERVE=<出力ディレクトリ (spec/testdata/golden/opencode-serve)> が要る (無ければ SKIP。CI は SKIP)。
// 非 root・bwrap・Landlock が要る (root では SKIP)。GORO_CAPTURE_RAW_DIR があれば、正規化の前の記録も、そこに書く (正規化の漏れの確認用。fixtures には入れない)。

// serveScene は、採取する 1 つの場面。
type serveScene struct {
	name, desc string
	steps      []openai.Step                        // 偽の provider の、会話の本体の応答 (呼ばれた順)
	wrap       func(main http.Handler) http.Handler // 会話の本体の provider を包む (遅延・壊れた応答)。nil なら、そのまま
	drive      func(r *sceneRun)                    // ホストの動き
	note       string                               // 採取の時に分かったこと (meta.json)
	// permissions は、POST /api/session に渡す permissions (JSON の配列)。空なら、全て ask。
	permissions string
}

// sceneRun は、1 つの場面の、ホスト側の状態。
type sceneRun struct {
	t   *testing.T
	c   *realCage
	sse *sseStream
	sid string
	cur int // 次に見る SSE イベントの位置
	// permissions は、newSession が session に渡す permissions (JSON の配列)。空なら、全て ask。
	permissions string
}

// executionEnded は、session.execution.* の終わり (started 以外) のイベントか。
func executionEnded(e sseEvent) bool {
	t := e.eventType()
	return strings.HasPrefix(t, "session.execution.") && t != "session.execution.started"
}

func (r *sceneRun) newSession() {
	r.t.Helper()
	perms := r.permissions
	if perms == "" {
		perms = `[{"action":"*","resource":"*","effect":"ask"}]`
	}
	st, body := r.c.call("POST", "/api/session", `{"permissions":`+perms+`}`)
	var s struct{ Data struct{ ID string } }
	json.Unmarshal([]byte(body), &s)
	if st != 200 || s.Data.ID == "" {
		r.t.Fatalf("POST /api/session = %d %s", st, body)
	}
	r.sid = s.Data.ID
}

func (r *sceneRun) prompt(text string) {
	r.t.Helper()
	b, _ := json.Marshal(map[string]string{"text": text})
	if st, body := r.c.call("POST", "/api/session/"+r.sid+"/prompt", string(b)); st/100 != 2 {
		r.t.Fatalf("prompt = %d %s", st, body)
	}
}

// until は、cur 以降のイベントで pred を満たす最初のものを待ち、cur をその次に進める。
func (r *sceneRun) until(what string, timeout time.Duration, pred func(sseEvent) bool) sseEvent {
	r.t.Helper()
	i := r.sse.waitFor(r.t, what, r.cur, timeout, pred)
	r.cur = i + 1
	return r.sse.snapshot()[i]
}

// permission は、permission.asked を待ち、その (session の ID, 許可の ID) を返す (サブエージェントの許可は、子の session の ID)。
func (r *sceneRun) permission() (sid, id string) {
	r.t.Helper()
	ev := r.until("permission.asked", 60*time.Second, func(e sseEvent) bool { return e.eventType() == "permission.asked" })
	var m struct {
		Data struct{ ID, SessionID string }
	}
	json.Unmarshal([]byte(ev.Data), &m)
	if m.Data.ID == "" {
		r.t.Fatalf("permission.asked に id が無い: %s", ev.Data)
	}
	return m.Data.SessionID, m.Data.ID
}

func (r *sceneRun) reply(sid, id, decision string) {
	r.t.Helper()
	b, _ := json.Marshal(map[string]string{"decision": decision})
	if st, body := r.c.call("POST", "/api/session/"+sid+"/permission/"+id+"/reply", string(b)); st/100 != 2 {
		r.t.Fatalf("permission reply = %d %s", st, body)
	}
}

// approveAll は、execution が終わるまで、permission.asked のたびに decision を返す。
func (r *sceneRun) approveAll(decision string) {
	r.t.Helper()
	for {
		ev := r.until("permission.asked か execution の終わり", 90*time.Second, func(e sseEvent) bool { return e.eventType() == "permission.asked" || executionEnded(e) })
		if executionEnded(ev) {
			return
		}
		var m struct {
			Data struct{ ID, SessionID string }
		}
		json.Unmarshal([]byte(ev.Data), &m)
		r.reply(m.Data.SessionID, m.Data.ID, decision)
	}
}

// settle は、execution の終わりを待つ。
func (r *sceneRun) settle() {
	r.t.Helper()
	r.until("execution の終わり", 90*time.Second, executionEnded)
}

// settleRetries は、provider の失敗を opencode が再試行し続けて (2.0.20 は 10 回ほど。全体で 90 秒ほど)、最後に諦めるまで待つ。
func (r *sceneRun) settleRetries() {
	r.t.Helper()
	r.until("execution の終わり (再試行のあと)", 240*time.Second, executionEnded)
}

func shellStep(cmd string) openai.Step {
	b, _ := json.Marshal(map[string]any{"command": cmd, "description": "run a command"})
	return openai.Step{ToolCalls: []openai.ToolCall{{ID: "call_1", Name: "shell", Arguments: b}}}
}

func toolStep(id, name string, args any) openai.Step {
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return openai.Step{ToolCalls: []openai.ToolCall{{ID: id, Name: name, Arguments: b}}}
}

func TestCaptureOpencodeServe(t *testing.T) {
	src, out := os.Getenv("GORO_REAL_OPENCODE"), os.Getenv("GORO_CAPTURE_OPENCODE_SERVE")
	if src == "" || out == "" {
		t.Skip("GORO_REAL_OPENCODE・GORO_CAPTURE_OPENCODE_SERVE (出力ディレクトリ) が要る")
	}
	only := os.Getenv("GORO_CAPTURE_SCENES") // 空なら全場面。コンマ区切りで絞る
	for _, sc := range serveScenes() {
		if only != "" && !strings.Contains(","+only+",", ","+sc.name+",") {
			continue
		}
		t.Run(sc.name, func(t *testing.T) { captureScene(t, src, out, sc) })
	}
}

func captureScene(t *testing.T, src, out string, sc serveScene) {
	llm := openai.NewServer(sc.steps...)
	titles := openai.NewServer(openai.Step{Content: "A short title"})
	var main http.Handler = llm
	if sc.wrap != nil {
		main = sc.wrap(llm)
	}
	c := startRealCage(t, src, splitProvider(main, titles))
	st, info := c.call("GET", "/api/info", "")
	var inf struct{ Version string }
	json.Unmarshal([]byte(info), &inf)
	if st != 200 || !strings.HasPrefix(inf.Version, "2.0.") {
		t.Fatalf("GET /api/info = %d %s", st, info)
	}
	r := &sceneRun{t: t, c: c, sse: c.openSSE(), permissions: sc.permissions}
	if raw := os.Getenv("GORO_CAPTURE_RAW_DIR"); raw != "" { // 正規化の前の記録 (失敗したときも書く)
		defer func() {
			var b bytes.Buffer
			for _, e := range r.sse.snapshot() {
				b.WriteString(e.Data + "\n")
			}
			os.MkdirAll(filepath.Join(raw, sc.name), 0o755)
			os.WriteFile(filepath.Join(raw, sc.name, "events.raw.ndjson"), b.Bytes(), 0o644)
			rc, _ := json.Marshal(c.recordedCalls())
			os.WriteFile(filepath.Join(raw, sc.name, "calls.raw.json"), rc, 0o644)
		}()
	}
	sc.drive(r)
	time.Sleep(700 * time.Millisecond) // 終わりのあとに続くイベントを拾う
	events, calls := r.sse.snapshot(), c.recordedCalls()

	// tools.json: opencode が provider に渡す tools (tool ごとの引数の正)。
	if sc.name == "simple-text" {
		if reqs := llm.Requests(); len(reqs) > 0 {
			var m struct {
				Tools json.RawMessage `json:"tools"`
			}
			json.Unmarshal(reqs[0].Body, &m)
			var pretty bytes.Buffer
			json.Indent(&pretty, m.Tools, "", "  ")
			pretty.WriteByte('\n')
			writeGolden(t, c.hostDir, out, filepath.Join(sc.name, "tools.json"), pretty.Bytes())
		}
	}
	c.stop()
	writeScene(t, c.hostDir, out, sc, inf.Version, events, calls)
}

// writeGolden は、out の下の rel に書く (ディレクトリは作る)。
func writeGolden(t *testing.T, hostDir, out, rel string, b []byte) {
	t.Helper()
	assertCleanFixture(t, hostDir, rel, b)
	p := filepath.Join(out, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// jsonOrString は、s が JSON ならその値、違えば文字列。
func jsonOrString(s string) any {
	if s == "" {
		return nil
	}
	var v any
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if d.Decode(&v) == nil && !d.More() {
		return v
	}
	return s
}

// writeScene は、場面の記録を、正規化して書く。番号の体系は、場面の中で共通 (要求 → イベントの順に、最初に現れた順)。
func writeScene(t *testing.T, hostDir, out string, sc serveScene, version string, events []sseEvent, calls []recordedCall) {
	t.Helper()
	norm := framenorm.New(framenorm.Serve)
	var reqs, evs bytes.Buffer
	for _, c := range calls {
		var segs []string
		path, query, hasQ := strings.Cut(c.Path, "?")
		for _, s := range strings.Split(path, "/") {
			segs = append(segs, norm.Symbol(s))
		}
		p := strings.Join(segs, "/")
		if hasQ {
			p += "?" + query
		}
		line, _ := json.Marshal(map[string]any{"method": c.Method, "path": p, "body": jsonOrString(c.Body), "status": c.Status, "response": jsonOrString(c.Response)})
		b, err := norm.Line(line)
		if err != nil {
			t.Fatal(err)
		}
		reqs.Write(b)
	}
	for _, e := range events {
		b, err := norm.Line([]byte(e.Data))
		if err != nil {
			t.Fatalf("イベントが JSON でない: %q: %v", e.Data, err)
		}
		evs.Write(b)
	}
	var un syscall.Utsname
	kernel := ""
	if syscall.Uname(&un) == nil {
		var rel []byte
		for _, ch := range un.Release {
			if ch == 0 {
				break
			}
			rel = append(rel, byte(ch))
		}
		kernel = string(rel)
	}
	meta, _ := json.MarshalIndent(map[string]any{
		"scene": sc.name, "description": sc.desc, "opencodeVersion": version, "capturedAt": time.Now().UTC().Format("2006-01-02"), "kernel": kernel,
		"events": len(events), "requests": len(calls), "note": sc.note,
	}, "", "  ")
	writeGolden(t, hostDir, out, filepath.Join(sc.name, "events.ndjson"), evs.Bytes())
	writeGolden(t, hostDir, out, filepath.Join(sc.name, "requests.ndjson"), reqs.Bytes())
	writeGolden(t, hostDir, out, filepath.Join(sc.name, "meta.json"), append(meta, '\n'))
}

// holdUntilCancelled は、provider の応答を返さず、要求が取り消される (opencode が中断する) まで待つ (30 秒で諦める)。
func holdUntilCancelled(main http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
			main.ServeHTTP(w, r)
		}
	})
}

// formID は、form.created を待ち、form の ID を返す。
func (r *sceneRun) formID() string {
	r.t.Helper()
	ev := r.until("form.created", 30*time.Second, func(e sseEvent) bool { return e.eventType() == "form.created" })
	var m struct {
		Data struct{ Form struct{ ID string } }
	}
	json.Unmarshal([]byte(ev.Data), &m)
	if m.Data.Form.ID == "" {
		r.t.Fatalf("form.created に form.id が無い: %s", ev.Data)
	}
	return m.Data.Form.ID
}

// questionTurn は、question の tool の 1 ターン: 承認 (once) → form → GET → 返答 (answer) → 終わりまで。
func questionTurn(r *sceneRun, answer map[string]any) {
	r.newSession()
	r.prompt("Ask me.")
	sid, id := r.permission()
	r.reply(sid, id, "once")
	fid := r.formID()
	r.c.call("GET", "/api/session/"+r.sid+"/form", "")
	b, _ := json.Marshal(map[string]any{"answer": answer})
	if st, body := r.c.call("POST", "/api/session/"+r.sid+"/form/"+fid+"/reply", string(b)); st/100 != 2 {
		r.t.Fatalf("form reply = %d %s", st, body)
	}
	r.approveAll("once")
}

func serveScenes() []serveScene {
	text := func(s string) openai.Step { return openai.Step{Content: s} }
	askOnce := func(r *sceneRun) { r.newSession(); r.prompt("Do it."); r.approveAll("once") }
	get := func(r *sceneRun, path string) { r.c.call("GET", path, "") }
	return []serveScene{
		{name: "simple-text", desc: "1 ターン。本文だけ (tool 無し)", steps: []openai.Step{text("Hello from the fake provider.")},
			drive: func(r *sceneRun) { r.newSession(); r.prompt("Say hello."); r.settle() }},
		{name: "tool-call-shell-allow", desc: "shell の tool 呼び出し → permission.asked → once → 結果のあとの本文", steps: []openai.Step{shellStep("echo goro-hi"), text("The command printed goro-hi.")},
			drive: askOnce},
		{name: "tool-call-shell-reject", note: `reject のあと、2 回目の provider の応答は来ず、tool が aborted で失敗し、execution は interrupted (reason: shutdown) で終わる。`, desc: "shell の tool 呼び出し → permission.asked → reject", steps: []openai.Step{shellStep("echo goro-hi"), text("Understood, I will not run it.")},
			drive: func(r *sceneRun) { r.newSession(); r.prompt("Do it."); r.approveAll("reject") }},
		{name: "tool-call-shell-always", note: `always を返すと、同じ rule (save: "echo *") の 2 回目は permission.asked が出ない。`, desc: "shell を 2 回: 1 回目に always → 2 回目は承認が出ないか", steps: []openai.Step{shellStep("echo one"),
			toolStep("call_2", "shell", map[string]any{"command": "echo two", "description": "run again"}), text("Both done.")},
			drive: func(r *sceneRun) { r.newSession(); r.prompt("Do it twice."); r.approveAll("always") }},
		{name: "pending-snapshot", desc: "承認待ちの間の GET (一覧・個別・session)。承認したあとの message の一覧", steps: []openai.Step{shellStep("echo goro-hi"), text("done")},
			drive: func(r *sceneRun) {
				r.newSession()
				r.prompt("Do it.")
				sid, id := r.permission()
				get(r, "/api/permission/request")
				get(r, "/api/session/"+sid+"/permission")
				get(r, "/api/session/"+sid+"/permission/"+id)
				get(r, "/api/session/"+sid)
				get(r, "/api/session/active")
				get(r, "/api/session/"+sid+"/inbox")
				r.reply(sid, id, "once")
				r.settle()
				get(r, "/api/permission/request")
				get(r, "/api/session/"+sid+"/message")
			}},
		{name: "actions", note: `write・edit の action は "edit" (metadata.files に patch)。/etc の read は external_directory → read の 2 連続の承認。execute は承認を要求しない。glob・grep は承認は出るが、檻に ripgrep が無く "ripgrep execution failed" で失敗する (成功の形は観察できない)。webfetch は fake provider の 404。`, desc: "各 tool を 1 つずつ呼び、once で承認する (action ごとの resources の形)", steps: []openai.Step{
			toolStep("call_write", "write", map[string]any{"path": "/work/notes.txt", "content": "hello\n"}),
			toolStep("call_edit", "edit", map[string]any{"path": "/work/notes.txt", "oldString": "hello", "newString": "goodbye"}),
			toolStep("call_read", "read", map[string]any{"path": "/work/notes.txt"}),
			toolStep("call_read_ext", "read", map[string]any{"path": "/etc/hostname"}),
			toolStep("call_glob", "glob", map[string]any{"pattern": "*.txt"}),
			toolStep("call_grep", "grep", map[string]any{"pattern": "goodbye"}),
			toolStep("call_webfetch", "webfetch", map[string]any{"url": "http://fake-provider.test/page", "format": "text"}),
			toolStep("call_skill", "skill", map[string]any{"id": "opencode"}),
			toolStep("call_execute", "execute", map[string]any{"code": "return 1 + 1"}),
			text("All tools were called."),
		}, drive: askOnce},
		{name: "subagent", note: `subagent の承認 (action subagent, resources [agent 名]) のあと、子の session (session.created に parentID・agent: general) が別に走る。子の承認は、子の session の ID で出る。親には session.tool.progress (metadata.sessionID) と、最後の session.tool.success (本文に <subagent sessionID=… state=completed>) が来る。`, desc: "subagent の tool 呼び出し: 子の session が shell を呼ぶ (承認は once)", steps: []openai.Step{
			toolStep("call_sub", "subagent", map[string]any{"agent": "general", "description": "list files", "prompt": "List the files in /work."}),
			toolStep("call_child_shell", "shell", map[string]any{"command": "ls /work", "description": "list"}),
			text("The directory is empty."),
			text("The subagent finished."),
		}, drive: askOnce},
		{name: "multi-turn", desc: "同じ session に 2 回 prompt する", steps: []openai.Step{text("First answer."), text("Second answer.")},
			drive: func(r *sceneRun) {
				r.newSession()
				r.prompt("First question.")
				r.settle()
				r.prompt("Second question.")
				r.settle()
				get(r, "/api/session/"+r.sid+"/message")
			}},
		{name: "question-single", note: `question の tool は、先に permission (action question, resources ["*"]) を要求し、承認のあとに form.created (metadata.kind: question。質問ごとの field: key q0, q1…) が出る。返答は POST /api/session/{sid}/form/{formID}/reply {"answer":{"q0":…}} (204)。2.0.20 に question.asked の系統は無い。`, desc: "question の tool (2.x): 承認 (action question) → form.created (kind question) → form の返答 (単一選択)", steps: []openai.Step{
			toolStep("call_q", "question", map[string]any{"questions": []map[string]any{{"question": "Which color?", "header": "Color", "options": []map[string]any{{"label": "Red", "description": "warm"}, {"label": "Blue", "description": "cool"}}}}}),
			text("You chose Red."),
		}, drive: func(r *sceneRun) { questionTurn(r, map[string]any{"q0": "Red"}) }},
		{name: "question-rule-allow", permissions: `[{"action":"*","resource":"*","effect":"ask"},{"action":"question","resource":"*","effect":"allow"}]`,
			note: `session の permissions に、"*" の ask のあとに、question の allow を足すと (あとの規則が勝つ)、question の tool の permission.asked が出ず、form.created が直接出る。順序を逆 (question の allow のあとに "*" の ask) にすると、あとの "*" が勝ち、permission.asked が出る (採取で確認。2.0.20)。`,
			desc: "question の tool: session の permissions で question を allow にした (承認の段が無い)", steps: []openai.Step{
				toolStep("call_q", "question", map[string]any{"questions": []map[string]any{{"question": "Which color?", "header": "Color", "options": []map[string]any{{"label": "Red", "description": "warm"}, {"label": "Blue", "description": "cool"}}}}}),
				text("You chose Red."),
			}, drive: func(r *sceneRun) {
				r.newSession()
				r.prompt("Ask me.")
				fid := r.formID()
				r.c.call("GET", "/api/session/"+r.sid+"/form", "")
				b, _ := json.Marshal(map[string]any{"answer": map[string]any{"q0": "Red"}})
				if st, body := r.c.call("POST", "/api/session/"+r.sid+"/form/"+fid+"/reply", string(b)); st/100 != 2 {
					r.t.Fatalf("form reply = %d %s", st, body)
				}
				r.approveAll("once")
			}},
		{name: "question-multiple", note: `multiple:true の質問は、field の type が multiselect (単一選択は string)。返答の値は配列 (["S","L"])。`, desc: "question の tool: 2 問 (単一選択・複数選択 multiple:true)。複数選択の返答は配列", steps: []openai.Step{
			toolStep("call_q", "question", map[string]any{"questions": []map[string]any{
				{"question": "Which color?", "header": "Color", "options": []map[string]any{{"label": "Red", "description": "warm"}, {"label": "Blue", "description": "cool"}}},
				{"question": "Which sizes?", "header": "Sizes", "multiple": true, "options": []map[string]any{{"label": "S", "description": "small"}, {"label": "M", "description": "medium"}, {"label": "L", "description": "large"}}},
			}}),
			text("Noted."),
		}, drive: func(r *sceneRun) { questionTurn(r, map[string]any{"q0": "Blue", "q1": []string{"S", "L"}}) }},
		{name: "question-custom", note: `選択肢に無い文字列を、そのまま返せる (field の custom: true)。`, desc: "question の tool: 選択肢に無い自由記述の返答 (custom)", steps: []openai.Step{
			toolStep("call_q", "question", map[string]any{"questions": []map[string]any{{"question": "Which color?", "header": "Color", "options": []map[string]any{{"label": "Red", "description": "warm"}, {"label": "Blue", "description": "cool"}}}}}),
			text("Green it is."),
		}, drive: func(r *sceneRun) { questionTurn(r, map[string]any{"q0": "Green (my own answer)"}) }},
		{name: "question-cancel", note: `DELETE /api/session/{sid}/form/{formID} (204) で form.cancelled → tool が aborted ("The user dismissed this question") → execution は interrupted (reason: shutdown)。`, desc: "question の tool: form を取り消す (DELETE)", steps: []openai.Step{
			toolStep("call_q", "question", map[string]any{"questions": []map[string]any{{"question": "Which color?", "header": "Color", "options": []map[string]any{{"label": "Red", "description": "warm"}, {"label": "Blue", "description": "cool"}}}}}),
			text("No answer, then."),
		}, drive: func(r *sceneRun) {
			r.newSession()
			r.prompt("Ask me.")
			sid, id := r.permission()
			r.reply(sid, id, "once")
			fid := r.formID()
			r.c.call("DELETE", "/api/session/"+r.sid+"/form/"+fid, "")
			r.approveAll("once")
		}},
		{name: "websearch-form", note: `websearch は承認のあとに、provider を選ぶ form (metadata.kind: websearch.provider。field choice: allow/choose/disable) を出す。答えないと 60 秒で form.cancelled (別の試行で観察)。ここでは DELETE で取り消した。`, desc: "websearch の tool: provider を選ぶ form (kind websearch.provider) が出る (承認 → form)", steps: []openai.Step{
			toolStep("call_ws", "websearch", map[string]any{"query": "goronation"}),
			text("Searched."),
		}, drive: func(r *sceneRun) {
			r.newSession()
			r.prompt("Search.")
			sid, id := r.permission()
			r.reply(sid, id, "once")
			fid := r.formID()
			get(r, "/api/form")
			r.c.call("DELETE", "/api/session/"+r.sid+"/form/"+fid, "")
			r.approveAll("once")
		}},
		{name: "interrupt-streaming", note: `POST /api/session/{sid}/interrupt → {"interrupted":true}。session.step.failed (aborted) → session.execution.interrupted (reason: user)。`, desc: "provider の応答の途中 (step.started のあと) で POST …/interrupt", steps: []openai.Step{text("never sent")},
			wrap: holdUntilCancelled,
			drive: func(r *sceneRun) {
				r.newSession()
				r.prompt("Say something long.")
				r.until("session.execution.started", 30*time.Second, func(e sseEvent) bool { return e.eventType() == "session.execution.started" })
				time.Sleep(1500 * time.Millisecond) // provider への要求が出て、応答を待っている間
				r.c.call("POST", "/api/session/"+r.sid+"/interrupt", "")
				r.settle()
			}},
		{name: "interrupt-pending-permission", note: `承認待ちの間の interrupt: permission.replied は出ず、tool は "Tool execution interrupted" で失敗し、未解決の承認は消える (GET /api/permission/request は空)。`, desc: "承認待ちの間に POST …/interrupt: 承認の要求の行方", steps: []openai.Step{shellStep("echo goro-hi"), text("unused")},
			drive: func(r *sceneRun) {
				r.newSession()
				r.prompt("Do it.")
				r.permission()
				r.c.call("POST", "/api/session/"+r.sid+"/interrupt", "")
				r.settle()
				get(r, "/api/permission/request")
			}},
		{name: "error-provider-500", note: `HTTP 500 は、opencode が 10 回ほど再試行 (session.retry.scheduled の attempt 2〜11。間隔は伸びる。全体で 90 秒ほど) してから session.execution.failed (provider.internal)。`, desc: "provider が HTTP 500 を返す", steps: []openai.Step{{Error: &openai.Error{Status: 500, Type: "api_error", Message: "fake provider failure"}}},
			drive: func(r *sceneRun) { r.newSession(); r.prompt("Hi."); r.settleRetries() }},
		{name: "error-provider-400", note: `HTTP 400 は再試行せず、すぐ session.execution.failed (provider.invalid-request)。`, desc: "provider が HTTP 400 (invalid_request_error) を返す", steps: []openai.Step{{Error: &openai.Error{Status: 400, Type: "invalid_request_error", Message: "fake bad request"}}},
			drive: func(r *sceneRun) { r.newSession(); r.prompt("Hi."); r.settle() }},
		{name: "error-provider-bad-stream", note: `JSON でない data 行は、再試行せず session.execution.failed (provider.invalid-output)。`, desc: "provider の応答が、壊れた SSE (JSON でない data 行)", steps: []openai.Step{text("unused")},
			wrap: func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					io.WriteString(w, "data: {this is not json\n\n")
				})
			},
			drive: func(r *sceneRun) { r.newSession(); r.prompt("Hi."); r.settle() }},
		{name: "error-provider-truncated", note: `[DONE]・finish_reason が無いまま切れた応答は、provider.invalid-output として再試行される (session.synthetic の "The previous response was interrupted…" を足して続きを求める)。10 回ほど繰り返して execution.failed。`, desc: "provider の応答が、途中で切れる ([DONE] が無い)", steps: []openai.Step{text("a long answer that gets cut")},
			wrap: func(main http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					io.WriteString(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
					w.(http.Flusher).Flush()
					panic(http.ErrAbortHandler)
				})
			},
			drive: func(r *sceneRun) { r.newSession(); r.prompt("Hi."); r.settleRetries() }},
	}
}

// 採取する場面 (serveScenes) と、CI が検査する fixtures の場面の一覧 (serveSceneNames) が、一致している。
func TestServeSceneNamesMatch(t *testing.T) {
	var names []string
	for _, sc := range serveScenes() {
		if sc.name == "" || sc.desc == "" || sc.drive == nil || len(sc.steps) == 0 {
			t.Errorf("場面 %q の定義が足りない", sc.name)
		}
		names = append(names, sc.name)
	}
	if !slices.Equal(names, serveSceneNames) {
		t.Errorf("serveScenes %v と serveSceneNames %v が違う", names, serveSceneNames)
	}
}

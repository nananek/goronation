package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/goronation/agent/claude"
	v0 "github.com/nananek/goronation/spec/v0"
)

// form・内容ハッシュ (ADR 0040・0042・0046・0047) の、Feed と Conversation のテスト。

// scriptStream は、決めた封筒を返し、受けたコマンドを記録する Stream (アダプタの代わり)。1 行 = {"type":…,"data":…} の封筒。
type scriptStream struct {
	mu   sync.Mutex
	cmds []v0.Command
}

func (s *scriptStream) DecodeFrame(raw []byte) ([]v0.Envelope, error) {
	var e v0.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	e.V, e.Durable = v0.Version, true
	return []v0.Envelope{e}, nil
}

func (s *scriptStream) EncodeCommand(cmd v0.Command) ([]byte, []v0.Envelope, error) {
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
	switch cmd.Type {
	case v0.CommandPrompt:
		return []byte("prompt\n"), nil, nil
	case v0.CommandFormResolve:
		var r v0.FormResolve
		json.Unmarshal(cmd.Data, &r)
		d, _ := json.Marshal(map[string]any{"by": "human", "outcome": r.Outcome, "request_id": r.RequestID})
		return []byte("form.resolve " + string(cmd.Data) + "\n"), []v0.Envelope{{V: v0.Version, Type: v0.TypeFormResolved, Durable: true, Data: d}}, nil
	case v0.CommandPermissionResolve:
		var r v0.PermissionResolve
		json.Unmarshal(cmd.Data, &r)
		d, _ := json.Marshal(map[string]any{"by": "human", "outcome": r.Outcome, "request_id": r.RequestID})
		return []byte("permission.resolve " + string(cmd.Data) + "\n"), []v0.Envelope{{V: v0.Version, Type: v0.TypePermissionResolved, Durable: true, Data: d}}, nil
	}
	return nil, nil, errors.New("unsupported")
}

func (s *scriptStream) commands() []v0.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]v0.Command(nil), s.cmds...)
}

func envLine(typ string, data any) []byte {
	d, _ := json.Marshal(data)
	b, _ := json.Marshal(map[string]any{"type": typ, "data": json.RawMessage(d)})
	return b
}

func permData(id string) map[string]any {
	return map[string]any{"request_id": id, "tool_name": "Bash", "kind": "execute", "input": map[string]any{"command": "ls"}, "summary": "Bash: ls",
		"details": []map[string]string{{"label": "command", "text": "ls", "kind": "command"}}}
}

// testForm は、select (options だけ・必須)・multiselect (custom・任意)・text の 3 つのフィールドを持つ form。
func testForm(id string) v0.FormRequested {
	return v0.FormRequested{RequestID: id, Kind: v0.FormKindQuestion, Fields: []v0.FormField{
		{Key: "color", Title: "c", Type: v0.FieldSelect, Required: true, Options: []v0.FormOption{{Label: "Red", Value: "red"}, {Label: "Blue", Value: "blue"}}},
		{Key: "langs", Type: v0.FieldMultiselect, Custom: true, Options: []v0.FormOption{{Label: "Go", Value: "go"}, {Label: "Rust", Value: "rust"}}},
		{Key: "note", Type: v0.FieldText},
	}}
}

// heldHash は、配られた requested の Event の content_hash。
func heldHash(t *testing.T, e *convEnv, typ, id string) string {
	t.Helper()
	for _, ev := range e.events(t) {
		if ev.Type != typ {
			continue
		}
		var v struct {
			Data struct {
				RequestID   string `json:"request_id"`
				ContentHash string `json:"content_hash"`
			}
		}
		if json.Unmarshal(ev.JSON, &v) == nil && v.Data.RequestID == id {
			return v.Data.ContentHash
		}
	}
	t.Fatalf("%s %s が配られていない", typ, id)
	return ""
}

func newFormConv(t *testing.T, require bool) (*convEnv, *scriptStream) {
	t.Helper()
	st := &scriptStream{}
	e := newConvWith(t, HubConfig{}, st, require)
	if err := e.c.Send("hi"); err != nil {
		t.Fatal(err)
	}
	return e, st
}

func answer(id string, kv map[string]v0.FormValue) v0.FormResolve {
	return v0.FormResolve{RequestID: id, Outcome: v0.FormAnswered, Answer: kv}
}

// ---- Feed: 検査と content_hash ----

func TestFeedSealsRequests(t *testing.T) {
	st := &scriptStream{}
	f := NewFeed(st, "s", fixedNow)
	// アダプタが付けた content_hash・語彙に無い欄は、上書き・落とされる。
	pd := permData("p1")
	pd["content_hash"] = "sha256:evil"
	pd["leak"] = "secret-from-agent-frame"
	evs, err := f.Decode(envLine(v0.TypePermissionRequested, pd))
	if err != nil || len(evs) != 1 || evs[0].Type != v0.TypePermissionRequested {
		t.Fatalf("%v %v", err, evs)
	}
	var got struct{ Data v0.PermissionRequested }
	if err := json.Unmarshal(evs[0].JSON, &got); err != nil {
		t.Fatal(err)
	}
	if got.Data.ContentHash == "" || got.Data.ContentHash == "sha256:evil" || got.Data.ContentHash != got.Data.Hash() {
		t.Fatalf("content_hash = %q", got.Data.ContentHash)
	}
	if strings.Contains(string(evs[0].JSON), "secret-from-agent-frame") {
		t.Errorf("語彙に無い欄が UI・API に出た: %s", evs[0].JSON)
	}
	// form も同じ。
	evs, err = f.Decode(envLine(v0.TypeFormRequested, testForm("f1")))
	if err != nil || len(evs) != 1 || evs[0].Type != v0.TypeFormRequested {
		t.Fatalf("%v %v", err, evs)
	}
	var fg struct{ Data v0.FormRequested }
	json.Unmarshal(evs[0].JSON, &fg)
	if fg.Data.ContentHash == "" || fg.Data.ContentHash != fg.Data.Hash() {
		t.Fatalf("form の content_hash = %q", fg.Data.ContentHash)
	}
	// 内容が 1 文字違えば、ハッシュが違う。
	other := testForm("f1")
	other.Fields[0].Title = "d"
	evs, _ = f.Decode(envLine(v0.TypeFormRequested, other))
	var og struct{ Data v0.FormRequested }
	json.Unmarshal(evs[0].JSON, &og)
	if og.Data.ContentHash == fg.Data.ContentHash {
		t.Error("内容が違うのに、同じハッシュ")
	}
}

// Validate を通らない要求 (アダプタをすり抜けた形) は、agent.frame (data 空) にして配る。
func TestFeedTurnsInvalidRequestsIntoFrames(t *testing.T) {
	f := NewFeed(&scriptStream{}, "s", fixedNow)
	badPerm := permData("p1")
	badPerm["input"] = []string{"not", "an", "object"}
	noFields := testForm("f1")
	noFields.Fields = nil
	dupKey := testForm("f2")
	dupKey.Fields[1].Key = "color"
	longLabel := testForm("f3")
	longLabel.Fields[0].Options[0].Label = strings.Repeat("a", v0.MaxFormLabelLen+1)
	for name, line := range map[string][]byte{
		"input がオブジェクトでない": envLine(v0.TypePermissionRequested, badPerm),
		"フィールドが無い":         envLine(v0.TypeFormRequested, noFields),
		"key が重複":          envLine(v0.TypeFormRequested, dupKey),
		"label が長い":        envLine(v0.TypeFormRequested, longLabel),
		"data が JSON でない":  []byte(`{"type":"form.requested","data":"x"}`),
	} {
		evs, err := f.Decode(line)
		if err != nil || len(evs) != 1 || evs[0].Type != v0.TypeAgentFrame || evs[0].Durable {
			t.Errorf("%s: %v %+v", name, err, evs)
			continue
		}
		if !strings.Contains(string(evs[0].JSON), `"data":{}`) {
			t.Errorf("%s: data が空でない: %s", name, evs[0].JSON)
		}
	}
	// 要約・詳細が無い旧い形は、そのまま通る (後方互換)。
	evs, err := f.Decode(envLine(v0.TypePermissionRequested, map[string]any{"request_id": "old", "tool_name": "Bash", "kind": "execute", "input": map[string]any{}}))
	if err != nil || evs[0].Type != v0.TypePermissionRequested {
		t.Errorf("旧い形: %v %v", err, evs)
	}
}

// ---- Conversation: form の回答 ----

func TestResolveFormAnswered(t *testing.T) {
	e, st := newFormConv(t, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	if e.c.Pending() != 1 || e.c.State() != StateAwaitingPermission || len(e.hub.pinned) != 1 {
		t.Fatalf("pending=%d state=%v pinned=%d", e.c.Pending(), e.c.State(), len(e.hub.pinned))
	}
	hash := heldHash(t, e, v0.TypeFormRequested, "f1")
	r := answer("f1", map[string]v0.FormValue{"color": v0.Text("red"), "langs": v0.Many("go", "zig"), "note": v0.Text("hi")})
	r.ContentHash = hash
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", r); err != nil {
		t.Fatal(err)
	}
	if e.c.Pending() != 0 || e.c.State() != StateTurn || len(e.hub.pinned) != 0 {
		t.Fatalf("pending=%d state=%v pinned=%d", e.c.Pending(), e.c.State(), len(e.hub.pinned))
	}
	// エージェントには、content_hash を渡さない。
	cmds := st.commands()
	last := cmds[len(cmds)-1]
	if last.Type != v0.CommandFormResolve || strings.Contains(string(last.Data), "content_hash") || strings.Contains(string(last.Data), hash) {
		t.Fatalf("command = %s %s", last.Type, last.Data)
	}
	var sent v0.FormResolve
	json.Unmarshal(last.Data, &sent)
	if sent.RequestID != "f1" || sent.Outcome != v0.FormAnswered || sent.Answer["langs"].Values[1] != "zig" {
		t.Fatalf("sent = %+v", sent)
	}
	if n := count(e.types(t), v0.TypeFormResolved); n != 1 {
		t.Fatalf("form.resolved が %d 件", n)
	}
	// 1 回限り。
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", r); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("2 回目: %v", err)
	}
}

func TestResolveFormCancelled(t *testing.T) {
	e, st := newFormConv(t, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", v0.FormResolve{RequestID: "f1", Outcome: v0.FormCancelled}); err != nil {
		t.Fatal(err)
	}
	if cmds := st.commands(); cmds[len(cmds)-1].Type != v0.CommandFormResolve || e.c.Pending() != 0 {
		t.Fatal("cancelled が書かれていない")
	}
}

// 不正な回答は、何も書かず、未決のまま残る。続けて、正しい回答が通る。
func TestBadFormAnswersWriteNothing(t *testing.T) {
	e, st := newFormConv(t, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	before := len(st.commands())
	writes := len(e.written())
	ok := map[string]v0.FormValue{"color": v0.Text("red")}
	bad := map[string]v0.FormResolve{
		"保持した form に無いキー":           answer("f1", map[string]v0.FormValue{"color": v0.Text("red"), "extra": v0.Text("x")}),
		"options に無い値 (custom でない)": answer("f1", map[string]v0.FormValue{"color": v0.Text("green")}),
		"必須の欠け":                     answer("f1", map[string]v0.FormValue{"note": v0.Text("x")}),
		"必須が空文字列":                   answer("f1", map[string]v0.FormValue{"color": v0.Text("")}),
		"select に配列":                answer("f1", map[string]v0.FormValue{"color": v0.Many("red")}),
		"multiselect に文字列":          answer("f1", map[string]v0.FormValue{"color": v0.Text("red"), "langs": v0.Text("go")}),
		"multiselect の重複":           answer("f1", map[string]v0.FormValue{"color": v0.Text("red"), "langs": v0.Many("go", "go")}),
		"multiselect の空要素":          answer("f1", map[string]v0.FormValue{"color": v0.Text("red"), "langs": v0.Many("")}),
		"長い値":                       answer("f1", map[string]v0.FormValue{"color": v0.Text("red"), "note": v0.Text(strings.Repeat("あ", v0.MaxFormAnswerText+1))}),
		"cancelled に answer":        {RequestID: "f1", Outcome: v0.FormCancelled, Answer: ok},
	}
	for name, r := range bad {
		if err := e.c.ResolveFormIn(e.c.Generation(), "f1", r); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", v0.FormResolve{RequestID: "f1", Outcome: "allow_once"}); !errors.Is(err, ErrBadOutcome) {
		t.Errorf("知らない outcome: %v", err)
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), "other", answer("f1", ok)); !errors.Is(err, ErrBadAnswer) { // URL・本文の request_id の食い違い
		t.Errorf("request_id の食い違い: %v", err)
	}
	if len(st.commands()) != before || len(e.written()) != writes || e.c.Pending() != 1 {
		t.Fatalf("不正な回答が、書かれた・未決が消えた (commands %d→%d・writes %d→%d・pending %d)", before, len(st.commands()), writes, len(e.written()), e.c.Pending())
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", answer("f1", ok)); err != nil {
		t.Fatalf("正しい回答: %v", err)
	}
}

// ---- 内容ハッシュ ----

func TestContentHashBinding(t *testing.T) {
	for _, require := range []bool{false, true} {
		t.Run(fmt.Sprintf("require=%v", require), func(t *testing.T) {
			e, st := newFormConv(t, require)
			_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
			_ = e.c.OnLine(envLine(v0.TypePermissionRequested, permData("p1")))
			fh, ph := heldHash(t, e, v0.TypeFormRequested, "f1"), heldHash(t, e, v0.TypePermissionRequested, "p1")
			if fh == "" || ph == "" || fh == ph {
				t.Fatalf("hash: %q %q", fh, ph)
			}
			ok := map[string]v0.FormValue{"color": v0.Text("red")}
			cmds := len(st.commands())
			// 違う値・別の要求のハッシュ・空 (require のとき)・ハッシュの接頭辞だけ: 何も書かず、未決のまま。
			for name, h := range map[string][2]string{ // {form への値, permission への値}
				"違う値":       {"sha256:00", "sha256:00"},
				"別の要求のハッシュ": {ph, fh},
				"接頭辞だけ":     {v0.HashPrefix, v0.HashPrefix},
				"大文字":       {strings.ToUpper(fh), strings.ToUpper(ph)},
			} {
				r := answer("f1", ok)
				r.ContentHash = h[0]
				if err := e.c.ResolveFormIn(e.c.Generation(), "f1", r); !errors.Is(err, ErrContentChanged) {
					t.Errorf("form %s: %v", name, err)
				}
				if err := e.c.ResolvePermissionIn(e.c.Generation(), v0.PermissionResolve{RequestID: "p1", Outcome: v0.AllowOnce, ContentHash: h[1]}); !errors.Is(err, ErrContentChanged) {
					t.Errorf("permission %s: %v", name, err)
				}
			}
			if err := e.c.ResolveFormIn(e.c.Generation(), "f1", answer("f1", ok)); require != errors.Is(err, ErrContentHashRequired) || (!require && err != nil) {
				t.Errorf("form の hash なし (require=%v): %v", require, err)
			}
			if err := e.c.ResolveIn(e.c.Generation(), "p1", v0.AllowOnce); require != errors.Is(err, ErrContentHashRequired) || (!require && err != nil) {
				t.Errorf("permission の hash なし (require=%v): %v", require, err)
			}
			if require { // ハッシュなしは拒否され、未決のまま。ハッシュがあれば通る。
				if len(st.commands()) != cmds || e.c.Pending() != 2 {
					t.Fatalf("書かれた・未決が消えた: %d→%d pending=%d", cmds, len(st.commands()), e.c.Pending())
				}
				r := answer("f1", ok)
				r.ContentHash = fh
				if err := e.c.ResolveFormIn(e.c.Generation(), "f1", r); err != nil {
					t.Fatal(err)
				}
				if err := e.c.ResolvePermissionIn(e.c.Generation(), v0.PermissionResolve{RequestID: "p1", Outcome: v0.RejectOnce, ContentHash: ph}); err != nil {
					t.Fatal(err)
				}
			}
			if e.c.Pending() != 0 {
				t.Fatalf("pending=%d", e.c.Pending())
			}
		})
	}
}

// ハッシュが合っていても、回答が不正なら通らない (ハッシュは、回答の検査の代わりにならない)。ハッシュが違えば、回答が正しくても通らない。
func TestHashAndAnswerAreBothChecked(t *testing.T) {
	e, _ := newFormConv(t, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	h := heldHash(t, e, v0.TypeFormRequested, "f1")
	r := answer("f1", map[string]v0.FormValue{"color": v0.Text("green")})
	r.ContentHash = h
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", r); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("hash が合って、回答が不正: %v", err)
	}
	r = answer("f1", map[string]v0.FormValue{"color": v0.Text("red")})
	r.ContentHash = "sha256:x"
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", r); !errors.Is(err, ErrContentChanged) {
		t.Errorf("hash が違って、回答が正しい: %v", err)
	}
}

// ---- 世代・種類の取り違え・並行 ----

func TestFormGenerationAndKindConfusion(t *testing.T) {
	e, st := newFormConv(t, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	_ = e.c.OnLine(envLine(v0.TypePermissionRequested, permData("p1")))
	ok := answer("f1", map[string]v0.FormValue{"color": v0.Text("red")})
	if err := e.c.ResolveFormIn("", "f1", ok); !errors.Is(err, ErrNoGeneration) {
		t.Errorf("世代なし: %v", err)
	}
	if err := e.c.ResolveFormIn("old-generation", "f1", ok); !errors.Is(err, ErrStaleGeneration) {
		t.Errorf("古い世代: %v", err)
	}
	if err := e.c.ResolvePermissionIn("", v0.PermissionResolve{RequestID: "p1", Outcome: v0.AllowOnce}); !errors.Is(err, ErrNoGeneration) {
		t.Errorf("permission の世代なし: %v", err)
	}
	// 種類の違う ID: form に permission の応答 (allow は、回答なしの許可になる)・権限に form の応答は、未決でないものとして扱う。
	cmds := len(st.commands())
	if err := e.c.ResolveIn(e.c.Generation(), "f1", v0.AllowOnce); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("form に permission の応答: %v", err)
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), "p1", v0.FormResolve{RequestID: "p1", Outcome: v0.FormCancelled}); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("権限に form の応答: %v", err)
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), "nope", v0.FormResolve{RequestID: "nope", Outcome: v0.FormCancelled}); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("未知の ID: %v", err)
	}
	if len(st.commands()) != cmds || e.c.Pending() != 2 {
		t.Fatalf("書かれた・未決が消えた")
	}
}

// 並行する 2 つの応答 (同じ form に、別の回答) は、1 つだけが通る。
func TestConcurrentFormResolutionsOnlyOneWins(t *testing.T) {
	e, st := newFormConv(t, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := "red"
			if i%2 == 1 {
				c = "blue"
			}
			errs[i] = e.c.ResolveFormIn(e.c.Generation(), "f1", answer("f1", map[string]v0.FormValue{"color": v0.Text(c)}))
		}()
	}
	wg.Wait()
	okN := 0
	for _, err := range errs {
		switch {
		case err == nil:
			okN++
		case !errors.Is(err, ErrAlreadyResolved):
			t.Errorf("err = %v", err)
		}
	}
	nForm := 0
	for _, c := range st.commands() {
		if c.Type == v0.CommandFormResolve {
			nForm++
		}
	}
	if okN != 1 || nForm != 1 {
		t.Fatalf("成功 %d・form.resolve %d (どちらも 1 のはず)", okN, nForm)
	}
}

// ---- 失効・上限・撤回 ----

func TestFormsExpireAsForms(t *testing.T) {
	for _, how := range []string{"stop", "close", "write-failure"} {
		t.Run(how, func(t *testing.T) {
			e, _ := newFormConv(t, false)
			_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
			_ = e.c.OnLine(envLine(v0.TypePermissionRequested, permData("p1")))
			switch how {
			case "stop":
				e.c.Stop()
			case "close":
				e.c.Close(0)
			case "write-failure": // 書けなくなった (別の応答の書き込みの失敗) → 会話が終わり、残りの未決も失効する
				e.mu.Lock()
				e.failW = true
				e.mu.Unlock()
				if err := e.c.ResolveIn(e.c.Generation(), "p1", v0.AllowOnce); !errors.Is(err, ErrWriteFailed) {
					t.Fatalf("%v", err)
				}
			}
			var form, perm int
			for _, ev := range e.events(t) {
				var v struct {
					Data struct{ By, Outcome, Request_id string }
				}
				json.Unmarshal(ev.JSON, &v)
				switch {
				case ev.Type == v0.TypeFormResolved && v.Data.Request_id == "f1" && v.Data.By == "policy" && v.Data.Outcome == "cancelled":
					form++
				case ev.Type == v0.TypePermissionResolved && v.Data.Request_id == "f1":
					t.Errorf("form が permission.resolved で閉じられた")
				case ev.Type == v0.TypePermissionResolved && v.Data.Request_id == "p1":
					perm++
				}
			}
			if form != 1 || perm != 1 || e.c.Pending() != 0 || len(e.hub.pinned) != 0 {
				t.Fatalf("form=%d perm=%d pending=%d pinned=%d", form, perm, e.c.Pending(), len(e.hub.pinned))
			}
			if err := e.c.ResolveFormIn(e.c.Generation(), "f1", v0.FormResolve{RequestID: "f1", Outcome: v0.FormCancelled}); err == nil {
				t.Error("失効した form に応答が通った")
			}
		})
	}
}

// form の応答の書き込みが失敗したら、会話を終え、決着 (by=policy・cancelled) を 1 つ付ける。
func TestFormResolveWriteFailure(t *testing.T) {
	e, _ := newFormConv(t, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	e.mu.Lock()
	e.failW = true
	e.mu.Unlock()
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", answer("f1", map[string]v0.FormValue{"color": v0.Text("red")})); !errors.Is(err, ErrWriteFailed) {
		t.Fatalf("%v", err)
	}
	n := 0
	for _, ev := range e.events(t) {
		if ev.Type == v0.TypeFormResolved {
			n++
		}
	}
	if n != 1 || e.c.State() != StateClosed || e.c.Pending() != 0 {
		t.Fatalf("form.resolved=%d state=%v pending=%d", n, e.c.State(), e.c.Pending())
	}
}

// ターンの外・上限超過の form は、エージェントに form.resolve (cancelled) を返し、by=policy の form.resolved で決着させる (権限の自動拒否の form 版)。
func TestFormsOutsideTurnOrOverLimitAreCancelled(t *testing.T) {
	st := &scriptStream{}
	e := newConvWith(t, HubConfig{}, st, false)
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("early"))) // ターンの外 (Send の前)
	if e.c.Pending() != 0 {
		t.Fatalf("ターンの外の form が未決になった")
	}
	if cmds := st.commands(); len(cmds) != 1 || cmds[0].Type != v0.CommandFormResolve || !strings.Contains(string(cmds[0].Data), `"outcome":"cancelled"`) {
		t.Fatalf("commands = %v", cmds)
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), "early", v0.FormResolve{RequestID: "early", Outcome: v0.FormCancelled}); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("自動で決着した form への後追い: %v", err)
	}
	_ = e.c.Send("hi")
	for i := 0; i < MaxPendingRequests; i++ { // 権限と form を混ぜて、上限まで
		if i%2 == 0 {
			_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm(fmt.Sprintf("f%d", i))))
		} else {
			_ = e.c.OnLine(envLine(v0.TypePermissionRequested, permData(fmt.Sprintf("p%d", i))))
		}
	}
	if e.c.Pending() != MaxPendingRequests {
		t.Fatalf("pending=%d", e.c.Pending())
	}
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("over")))
	_ = e.c.OnLine(envLine(v0.TypePermissionRequested, permData("overp")))
	if e.c.Pending() != MaxPendingRequests {
		t.Fatalf("上限を超えて未決になった: %d", e.c.Pending())
	}
	var overForm, overPerm string
	for _, ev := range e.events(t) {
		var v struct {
			Data struct{ By, Outcome, Request_id string }
		}
		json.Unmarshal(ev.JSON, &v)
		if ev.Type == v0.TypeFormResolved && v.Data.Request_id == "over" {
			overForm = v.Data.By + "/" + v.Data.Outcome
		}
		if ev.Type == v0.TypePermissionResolved && v.Data.Request_id == "overp" {
			overPerm = v.Data.By + "/" + v.Data.Outcome
		}
	}
	if overForm != "policy/cancelled" || overPerm != "policy/reject_once" {
		t.Fatalf("form=%q perm=%q", overForm, overPerm)
	}
}

// 固定: 未決の form は、リングから溢れても、新しい購読者の Snapshot に残る。決着で外れる。
func TestPendingFormSurvivesRingOverflow(t *testing.T) {
	st := &scriptStream{}
	e := newConvWith(t, HubConfig{MaxBytes: 2048}, st, false)
	_ = e.c.Send("hi")
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	for i := 0; i < 50; i++ {
		_ = e.c.OnLine(envLine(v0.TypeMessageText, map[string]string{"text": strings.Repeat("x", 200)}))
	}
	if count(e.types(t), v0.TypeFormRequested) != 1 {
		t.Fatalf("溢れた後の Snapshot に、未決の form が無い")
	}
	_ = e.c.ResolveFormIn(e.c.Generation(), "f1", v0.FormResolve{RequestID: "f1", Outcome: v0.FormCancelled})
	if count(e.types(t), v0.TypeFormRequested) != 0 {
		t.Fatalf("決着した form が、固定されたまま")
	}
}

// エージェントの撤回 (form.resolved by=agent) は、未決の form を外し、固定を解く。会話が先に決着させた form の後追いの決着は、配らない。
func TestAgentCancelsPendingForm(t *testing.T) {
	st := &scriptStream{}
	e := newConvWith(t, HubConfig{}, st, false)
	_ = e.c.Send("hi")
	_ = e.c.OnLine(envLine(v0.TypeFormRequested, testForm("f1")))
	_ = e.c.OnLine(envLine(v0.TypeFormResolved, map[string]string{"by": "agent", "outcome": "cancelled", "request_id": "f1"}))
	if e.c.Pending() != 0 || len(e.hub.pinned) != 0 || count(e.types(t), v0.TypeFormResolved) != 1 {
		t.Fatalf("pending=%d pinned=%d", e.c.Pending(), len(e.hub.pinned))
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), "f1", v0.FormResolve{RequestID: "f1", Outcome: v0.FormCancelled}); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("撤回のあとの応答: %v", err)
	}
	// Stop で先に決着させた form に、エージェントが後から決着を出しても、2 つ目は配らない。
	e2 := newConvWith(t, HubConfig{}, &scriptStream{}, false)
	_ = e2.c.Send("hi")
	_ = e2.c.OnLine(envLine(v0.TypeFormRequested, testForm("f2")))
	e2.c.Stop()
	_ = e2.c.OnLine(envLine(v0.TypeFormResolved, map[string]string{"by": "agent", "outcome": "cancelled", "request_id": "f2"}))
	if n := count(e2.types(t), v0.TypeFormResolved); n != 1 {
		t.Fatalf("form.resolved が %d 件 (ちょうど 1 件のはず)", n)
	}
}

// ---- 実際の claude のアダプタ (AskUserQuestion の golden) を通した、Conversation の E2E ----

func TestClaudeAskUserQuestionEndToEnd(t *testing.T) {
	e := newConv(t, HubConfig{})
	if err := e.c.Send("ask me"); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, l := range goldenLines(t, "ask-user-question-multi") {
		_ = e.c.OnLine(l)
		if isRequest(l) && id == "" {
			for _, ev := range e.events(t) {
				if ev.Type == v0.TypeFormRequested {
					id, _ = requestID(ev)
				}
			}
			break
		}
	}
	if id == "" || e.c.Pending() != 1 {
		t.Fatalf("form が未決にならない (id=%q pending=%d)", id, e.c.Pending())
	}
	for _, ev := range e.events(t) {
		if ev.Type == v0.TypePermissionRequested {
			t.Fatalf("AskUserQuestion が permission.requested になった")
		}
	}
	// 権限の応答は通らない。保持した質問に無い値は通らない (custom は許す)。
	if err := e.c.ResolveIn(e.c.Generation(), id, v0.AllowOnce); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("form に allow_once: %v", err)
	}
	h := heldHash(t, e, v0.TypeFormRequested, id)
	if err := e.c.ResolveFormIn(e.c.Generation(), id, v0.FormResolve{RequestID: id, Outcome: v0.FormAnswered, ContentHash: h,
		Answer: map[string]v0.FormValue{"q9": v0.Text("x")}}); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("知らないキー: %v", err)
	}
	if err := e.c.ResolveFormIn(e.c.Generation(), id, v0.FormResolve{RequestID: id, Outcome: v0.FormAnswered, ContentHash: h,
		Answer: map[string]v0.FormValue{"q0": v0.Text("青"), "q1": v0.Many("Go", "Rust")}}); err != nil {
		t.Fatal(err)
	}
	w := e.written()
	last := w[len(w)-1]
	var o struct {
		Response struct {
			Request_id string
			Response   struct {
				Behavior     string
				UpdatedInput struct{ Answers map[string]string }
			}
		}
	}
	if err := json.Unmarshal([]byte(last), &o); err != nil {
		t.Fatal(err)
	}
	a := o.Response.Response.UpdatedInput.Answers
	if o.Response.Request_id != id || o.Response.Response.Behavior != "allow" || a["好きな色は?"] != "青" || a["使う言語は? (複数可)"] != "Go, Rust" {
		t.Fatalf("control_response = %s", last)
	}
	if e.c.Pending() != 0 || count(e.types(t), v0.TypeFormResolved) != 1 {
		t.Fatal("決着していない")
	}
	_ = claude.Name
}

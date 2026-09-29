package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/goronation/agent/claude"
	v0 "github.com/nananek/goronation/spec/v0"
)

type convEnv struct {
	c      *Conversation
	hub    *Hub
	mu     sync.Mutex
	writes []string
	failW  bool
	stops  int
}

func newConv(t *testing.T, hubCfg HubConfig) *convEnv {
	t.Helper()
	e := &convEnv{hub: NewHub(hubCfg)}
	e.c = NewConversation(ConversationConfig{
		Feed: NewFeed(claude.Adapter{}.NewStream(), "s", fixedNow),
		Hub:  e.hub,
		Write: func(line []byte) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.failW {
				return errors.New("EPIPE")
			}
			e.writes = append(e.writes, string(line))
			return nil
		},
		OnStop: func() { e.mu.Lock(); e.stops++; e.mu.Unlock() },
	})
	return e
}

func (e *convEnv) written() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.writes...)
}

// events は、Hub の Snapshot の type の列 (固定した Event を含む)。
func (e *convEnv) events(t *testing.T) []Event {
	t.Helper()
	s, err := e.hub.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	return s.Snapshot
}

func (e *convEnv) types(t *testing.T) []string {
	var out []string
	for _, ev := range e.events(t) {
		out = append(out, ev.Type)
	}
	return out
}

func count(types []string, typ string) int {
	n := 0
	for _, x := range types {
		if x == typ {
			n++
		}
	}
	return n
}

func reqFrame(id string) []byte {
	return []byte(fmt.Sprintf(`{"type":"control_request","request_id":%q,"request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"tu-%s","input":{"command":"ls"}}}`, id, id))
}

func goldenLines(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "spec", "testdata", "golden", "claude", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		out = append(out, l)
	}
	return out
}

func isRequest(l []byte) bool { return bytes.Contains(l, []byte(`"can_use_tool"`)) }

// resolvedBy は、request_id の permission.resolved の (by, outcome)。
func resolvedBy(t *testing.T, evs []Event, id string) (by, outcome string, ok bool) {
	t.Helper()
	for _, e := range evs {
		if e.Type != v0.TypePermissionResolved {
			continue
		}
		var v struct {
			Data struct{ By, Outcome, Request_id string }
		}
		if err := json.Unmarshal(e.JSON, &v); err != nil {
			t.Fatal(err)
		}
		if v.Data.Request_id == id {
			return v.Data.By, v.Data.Outcome, true
		}
	}
	return "", "", false
}

func onlyPendingID(t *testing.T, e *convEnv) string {
	t.Helper()
	var ids []string
	for _, ev := range e.events(t) {
		if ev.Type == v0.TypePermissionRequested {
			id, _ := requestID(ev)
			if _, _, done := resolvedBy(t, e.events(t), id); !done {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) != 1 {
		t.Fatalf("未決の要求が 1 件でない: %v", ids)
	}
	return ids[0]
}

// 要求 → 承認 → tool 実行 → 完了 / 要求 → 拒否 の golden の流れを、状態機械の上で再生する。
func TestGoldenFlows(t *testing.T) {
	for _, tc := range []struct{ name, outcome, want string }{
		{"permission-interactive-allow", v0.AllowOnce, `"behavior":"allow"`},
		{"permission-interactive-deny", v0.RejectOnce, `"behavior":"deny"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newConv(t, HubConfig{})
			if err := e.c.Send("hi"); err != nil || e.c.State() != StateTurn {
				t.Fatalf("send: %v state=%v", err, e.c.State())
			}
			var id string
			for _, l := range goldenLines(t, tc.name) {
				if err := e.c.OnLine(l); err != nil {
					t.Fatal(err)
				}
				if isRequest(l) {
					if e.c.State() != StateAwaitingPermission {
						t.Fatalf("state=%v", e.c.State())
					}
					id = onlyPendingID(t, e)
					if err := e.c.Resolve(id, tc.outcome); err != nil {
						t.Fatal(err)
					}
					if e.c.Pending() != 0 || e.c.State() != StateTurn {
						t.Fatalf("pending=%d state=%v", e.c.Pending(), e.c.State())
					}
					if err := e.c.Resolve(id, tc.outcome); !errors.Is(err, ErrAlreadyResolved) {
						t.Fatalf("2 回目: %v", err)
					}
				}
				if bytes.Contains(l, []byte(`"type":"result"`)) {
					break // 2 ターン目 (追いプロンプト) は、ここでは見ない
				}
			}
			if id == "" {
				t.Fatal("要求が無い")
			}
			w := e.written()
			if len(w) != 2 || !strings.Contains(w[0], `"initialize"`) || !strings.Contains(w[1], tc.want) || !strings.Contains(w[1], id) {
				t.Fatalf("writes=%q", w)
			}
			if by, out, ok := resolvedBy(t, e.events(t), id); !ok || by != "human" || out != tc.outcome {
				t.Fatalf("resolved by=%q outcome=%q ok=%v", by, out, ok)
			}
			if e.c.State() != StateIdle {
				t.Fatalf("state=%v", e.c.State())
			}
			if err := e.c.Send("again"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// claude が撤回した (control_cancel_request) 要求は、決着済みで、後追いの応答は 409 (書き込まない)。
func TestGoldenInterrupt(t *testing.T) {
	e := newConv(t, HubConfig{})
	if err := e.c.Send("hi"); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, l := range goldenLines(t, "permission-interactive-interrupt") {
		if err := e.c.OnLine(l); err != nil {
			t.Fatal(err)
		}
		if isRequest(l) {
			id = onlyPendingID(t, e)
		}
	}
	if by, out, ok := resolvedBy(t, e.events(t), id); !ok || by != "agent" || out != "cancelled" {
		t.Fatalf("resolved by=%q outcome=%q ok=%v", by, out, ok)
	}
	if err := e.c.Resolve(id, v0.AllowOnce); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("撤回後の応答: %v", err)
	}
	if len(e.written()) != 1 { // initialize + prompt の 1 回だけ
		t.Fatalf("writes=%q", e.written())
	}
}

func TestResolveErrors(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("r1"))
	for _, oc := range []string{v0.AllowAlways, v0.RejectAlways, "", "cancelled", "ALLOW_ONCE"} {
		if err := e.c.Resolve("r1", oc); !errors.Is(err, ErrBadOutcome) {
			t.Errorf("outcome %q: %v", oc, err)
		}
	}
	if err := e.c.Resolve("nope", v0.AllowOnce); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("未知の ID: %v", err)
	}
	if e.c.Pending() != 1 {
		t.Fatal("拒否された応答が、未決を消した")
	}
	if err := e.c.Resolve("r1", v0.RejectOnce); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Resolve("r1", v0.AllowOnce); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("決着後: %v", err)
	}
	// 決着済みの ID の再要求は、新しい要求にならない (Stream が agent.frame にする)。
	_ = e.c.OnLine(reqFrame("r1"))
	if e.c.Pending() != 0 || count(e.types(t), v0.TypePermissionRequested) != 1 {
		t.Fatalf("再要求が通った: pending=%d %v", e.c.Pending(), e.types(t))
	}
}

// 承認と拒否を、並行に 2 つ出しても、1 つだけが通り、claude への書き込みは 1 回。
func TestConcurrentResolveOnlyOneWins(t *testing.T) {
	for i := 0; i < 200; i++ {
		e := newConv(t, HubConfig{})
		_ = e.c.Send("hi")
		_ = e.c.OnLine(reqFrame("r"))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, oc := range []string{v0.AllowOnce, v0.RejectOnce} {
			wg.Add(1)
			go func() { defer wg.Done(); errs[j] = e.c.Resolve("r", oc) }()
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("errs=%v", errs)
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, ErrAlreadyResolved) {
				t.Fatalf("err=%v", err)
			}
		}
		if len(e.written()) != 2 { // initialize+prompt と、control_response
			t.Fatalf("writes=%q", e.written())
		}
	}
}

func TestSendRules(t *testing.T) {
	e := newConv(t, HubConfig{})
	if err := e.c.Send(""); !errors.Is(err, ErrEmptyText) {
		t.Fatal(err)
	}
	if err := e.c.Send(strings.Repeat("a", MaxMessageBytes+1)); !errors.Is(err, ErrTextTooLong) {
		t.Fatal(err)
	}
	if err := e.c.Send("a\xffb"); !errors.Is(err, ErrBadText) {
		t.Fatal(err)
	}
	if len(e.written()) != 0 || e.c.State() != StateIdle {
		t.Fatal("拒否した Send が状態を変えた")
	}
	if err := e.c.Send(strings.Repeat("a", MaxMessageBytes)); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Send("second"); !errors.Is(err, ErrBusy) {
		t.Fatalf("ターン中: %v", err)
	}
	_ = e.c.OnLine(reqFrame("r"))
	if err := e.c.Send("third"); !errors.Is(err, ErrBusy) {
		t.Fatalf("承認待ち: %v", err)
	}
	if len(e.written()) != 1 {
		t.Fatalf("writes=%d", len(e.written()))
	}
}

// 未決が上限を超えた要求は、見せたうえで、claude に拒否を返し、by=policy で決着する。上限内の要求は残る。
func TestPendingCapAutoRejects(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	for i := 0; i < MaxPendingRequests+3; i++ {
		_ = e.c.OnLine(reqFrame(fmt.Sprintf("r%d", i)))
	}
	if e.c.Pending() != MaxPendingRequests {
		t.Fatalf("pending=%d", e.c.Pending())
	}
	evs := e.events(t)
	for i := 0; i < MaxPendingRequests+3; i++ {
		id := fmt.Sprintf("r%d", i)
		by, out, ok := resolvedBy(t, evs, id)
		if i < MaxPendingRequests && ok {
			t.Errorf("%s が決着した", id)
		}
		if i >= MaxPendingRequests && (!ok || by != "policy" || out != v0.RejectOnce) {
			t.Errorf("%s: by=%q out=%q ok=%v", id, by, out, ok)
		}
	}
	w := e.written()
	if len(w) != 1+3 || strings.Count(strings.Join(w[1:], ""), `"behavior":"deny"`) != 3 {
		t.Fatalf("writes=%q", w)
	}
	if err := e.c.Resolve(fmt.Sprintf("r%d", MaxPendingRequests), v0.AllowOnce); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("自動拒否した要求への承認: %v", err)
	}
}

// ターンの外に来た要求は、承認の対象にしない。
func TestRequestOutsideTurnIsRejected(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.OnLine(reqFrame("stray"))
	if e.c.Pending() != 0 {
		t.Fatal("ターンの外の要求が、未決になった")
	}
	if by, _, ok := resolvedBy(t, e.events(t), "stray"); !ok || by != "policy" {
		t.Fatalf("by=%q ok=%v", by, ok)
	}
	if err := e.c.Resolve("stray", v0.AllowOnce); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatal(err)
	}
}

// 未決を残したまま result (ターンの終わり) が来たら、要求は閉じ、idle に戻る。ターンの外の result は、状態を変えない。
func TestTurnCompletedWithPendingAndStrayResult(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("r"))
	_ = e.c.OnLine([]byte(`{"type":"result","is_error":false,"result":"x"}`))
	if e.c.State() != StateIdle || e.c.Pending() != 0 {
		t.Fatalf("state=%v pending=%d", e.c.State(), e.c.Pending())
	}
	if by, out, ok := resolvedBy(t, e.events(t), "r"); !ok || by != "agent" || out != "cancelled" {
		t.Fatalf("by=%q out=%q ok=%v", by, out, ok)
	}
	if err := e.c.Resolve("r", v0.AllowOnce); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatal(err)
	}
	_ = e.c.OnLine([]byte(`{"type":"result","is_error":false,"result":"stray"}`))
	if e.c.State() != StateIdle {
		t.Fatalf("state=%v", e.c.State())
	}
}

func TestStop(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("r"))
	e.c.Stop()
	e.c.Stop()
	if e.stops != 1 {
		t.Fatalf("OnStop が %d 回", e.stops)
	}
	if by, out, ok := resolvedBy(t, e.events(t), "r"); !ok || by != "policy" || out != "cancelled" {
		t.Fatalf("by=%q out=%q ok=%v", by, out, ok)
	}
	if err := e.c.Send("x"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := e.c.Resolve("r", v0.AllowOnce); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("停止後の承認は 404: %v", err)
	}
	// 停止の途中に来た要求は、書き込まずに (入力は閉じた)、cancelled にする。
	before := len(e.written())
	_ = e.c.OnLine(reqFrame("late"))
	if e.c.Pending() != 0 || len(e.written()) != before {
		t.Fatal("停止後の要求が、未決・書き込みになった")
	}
	if _, out, ok := resolvedBy(t, e.events(t), "late"); !ok || out != "cancelled" {
		t.Fatalf("out=%q ok=%v", out, ok)
	}
	e.c.Close(3)
	s, _ := e.hub.Subscribe()
	if _, err := s.Next(context.Background()); err == nil || s.Exit() != 3 {
		t.Fatalf("hub が終わっていない: %v exit=%d", err, s.Exit())
	}
}

func TestCloseExpiresPending(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("a"))
	_ = e.c.OnLine(reqFrame("b"))
	e.c.Close(0)
	e.c.Close(1) // 2 回目は何もしない
	evs := e.events(t)
	for _, id := range []string{"a", "b"} {
		if by, out, ok := resolvedBy(t, evs, id); !ok || by != "policy" || out != "cancelled" {
			t.Fatalf("%s: by=%q out=%q ok=%v", id, by, out, ok)
		}
		if err := e.c.Resolve(id, v0.AllowOnce); !errors.Is(err, ErrUnknownRequest) {
			t.Fatalf("終了後の承認は 404: %v", err)
		}
	}
	if e.c.State() != StateClosed || e.c.Send("x") == nil {
		t.Fatal("終了後に Send できた")
	}
}

// 書き込みの失敗: 会話を終え (やり直さない)、turn.started は配らず、error を残す。
func TestWriteFailure(t *testing.T) {
	e := newConv(t, HubConfig{})
	e.failW = true
	if err := e.c.Send("hi"); !errors.Is(err, ErrWriteFailed) {
		t.Fatal(err)
	}
	if e.c.State() != StateClosed || count(e.types(t), v0.TypeTurnStarted) != 0 || count(e.types(t), v0.TypeError) != 1 {
		t.Fatalf("state=%v %v", e.c.State(), e.types(t))
	}
	if err := e.c.Send("again"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}

	e = newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("r"))
	e.failW = true
	if err := e.c.Resolve("r", v0.AllowOnce); !errors.Is(err, ErrWriteFailed) {
		t.Fatal(err)
	}
	if e.c.Pending() != 0 || e.c.State() != StateClosed {
		t.Fatal("書けなかった要求が残った")
	}
	if _, out, ok := resolvedBy(t, e.events(t), "r"); !ok || out != "cancelled" {
		t.Fatalf("out=%q ok=%v", out, ok)
	}
}

// 未決の要求は、リングから溢れても、新しい購読者の Snapshot に残り、決着したら消える (L4)。
func TestPendingRequestSurvivesRingEviction(t *testing.T) {
	e := newConv(t, HubConfig{MaxBytes: 4096})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("keep"))
	for i := 0; i < 200; i++ { // 小さな行の洪水で、リングを何度も入れ替える
		_ = e.c.OnLine([]byte(`{"type":"nothing"}`))
	}
	snap := func() []Event { return e.events(t) }
	has := func(evs []Event) bool {
		for _, ev := range evs {
			if id, _ := requestID(ev); ev.Type == v0.TypePermissionRequested && id == "keep" {
				return true
			}
		}
		return false
	}
	evs := snap()
	if !has(evs) {
		t.Fatal("溢れた未決の要求が、Snapshot に無い")
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("Snapshot が Seq 順でない: %d の後に %d", evs[i-1].Seq, evs[i].Seq)
		}
	}
	if err := e.c.Resolve("keep", v0.RejectOnce); err != nil {
		t.Fatal(err)
	}
	if has(snap()) {
		t.Fatal("決着した要求が、固定されたまま")
	}
}

// 要求の洪水でも、固定する Event の数は、MaxPendingRequests まで。
func TestPinnedCountIsBounded(t *testing.T) {
	e := newConv(t, HubConfig{MaxBytes: 2048})
	_ = e.c.Send("hi")
	for i := 0; i < 500; i++ {
		_ = e.c.OnLine(reqFrame(fmt.Sprintf("r%d", i)))
	}
	e.hub.mu.Lock()
	n := len(e.hub.pinned)
	e.hub.mu.Unlock()
	if n != MaxPendingRequests {
		t.Fatalf("pinned=%d", n)
	}
}

type errStream struct{}

func (errStream) DecodeFrame([]byte) ([]v0.Envelope, error) {
	return []v0.Envelope{{V: v0.Version, Type: v0.TypeError, Durable: true, Data: json.RawMessage(`{"message":"limit"}`)}}, nil
}
func (errStream) EncodeCommand(v0.Command) ([]byte, []v0.Envelope, error) { return nil, nil, nil }

// 同じ error の連続は、最初の 1 件だけを配る。
func TestErrorFloodIsCoalesced(t *testing.T) {
	hub := NewHub(HubConfig{})
	c := NewConversation(ConversationConfig{Feed: NewFeed(errStream{}, "s", fixedNow), Hub: hub, Write: func([]byte) error { return nil }})
	for i := 0; i < 1000; i++ {
		_ = c.OnLine([]byte(`x`))
	}
	s, _ := hub.Subscribe()
	if len(s.Snapshot) != 1 {
		t.Fatalf("error が %d 件", len(s.Snapshot))
	}
}

// 変換できない行は、何も配らず、状態を変えない。
func TestOnLineDecodeError(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	before := len(e.events(t))
	if err := e.c.OnLine([]byte(`not json`)); err == nil {
		t.Fatal("error が返らない")
	}
	if len(e.events(t)) != before || e.c.State() != StateTurn {
		t.Fatal("状態が変わった")
	}
}

// Send・OnLine・Resolve・Stop を並行に回す (-race)。
func TestConversationRace(t *testing.T) {
	e := newConv(t, HubConfig{MaxBytes: 8192})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := fmt.Sprintf("g%d-%d", g, i)
				_ = e.c.Send("x")
				_ = e.c.OnLine(reqFrame(id))
				_ = e.c.Resolve(id, v0.AllowOnce)
				_ = e.c.OnLine([]byte(`{"type":"result","is_error":false}`))
				if i == 150 && g == 0 {
					e.c.Stop()
				}
			}
		}()
	}
	wg.Wait()
	e.c.Close(0)
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// 偽の claude (chat セッションの stream-json): 標準入力を 1 行ずつ読み、initialize には control_response で答え、ユーザーの指示 (user) ごとに
// 「メッセージ → Write の tool_use → can_use_tool の control_request」を出して、応答 (control_response) を待ち、受けた応答の内容
// (behavior と、許可した input) を、メッセージに書いてから、result で終える。最後の引数が場面: "flow" (この動き)・"hold" (何も返さず読み続ける)。
func fakeChat(args []string) int {
	mode := args[len(args)-1]
	if mode == "spoof" {
		return fakeChatSpoof()
	}
	return runFakeChat(mode, os.Stdin, os.Stdout)
}

// runFakeChat は、fakeChat の本体 (標準入出力を、引数にしたもの。檻を使わないテストが、プロセス内で動かせる)。
func runFakeChat(mode string, stdin io.Reader, stdout io.Writer) int {
	in := bufio.NewScanner(stdin)
	in.Buffer(nil, 8<<20)
	out := bufio.NewWriter(stdout)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	next := func() (map[string]any, bool) {
		for in.Scan() {
			var m map[string]any
			if json.Unmarshal(in.Bytes(), &m) == nil {
				return m, true
			}
		}
		return nil, false
	}
	assistant := func(id string, content ...any) map[string]any {
		return map[string]any{"type": "assistant", "session_id": "sess-1", "parent_tool_use_id": nil,
			"message": map[string]any{"id": id, "role": "assistant", "model": "fake", "content": content, "stop_reason": nil,
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}
	}
	turn := 0
	for {
		m, ok := next()
		if !ok {
			return 0
		}
		switch m["type"] {
		case "control_request":
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m["request_id"], "response": map[string]any{}}})
		case "user":
			if mode == "hold" {
				continue
			}
			turn++
			toolID, reqID := fmt.Sprintf("toolu_%d", turn), fmt.Sprintf("perm-%d", turn)
			input := map[string]any{"file_path": "/work/new.txt", "content": fmt.Sprintf("turn %d\n", turn)}
			emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-1", "cwd": "/work", "model": "fake", "tools": []string{"Write"}})
			emit(assistant(fmt.Sprintf("m%da", turn), map[string]any{"type": "text", "text": "書きます。"}))
			emit(assistant(fmt.Sprintf("m%db", turn), map[string]any{"type": "tool_use", "id": toolID, "name": "Write", "input": input}))
			emit(map[string]any{"type": "control_request", "request_id": reqID, "request": map[string]any{
				"subtype": "can_use_tool", "tool_name": "Write", "input": input, "tool_use_id": toolID, "display_name": "Write", "description": "new.txt"}})
			var got map[string]any
			for {
				r, ok := next()
				if !ok {
					return 0
				}
				if r["type"] == "control_response" {
					got = r
					break
				}
			}
			resp, _ := got["response"].(map[string]any)
			body, _ := resp["response"].(map[string]any)
			seen, _ := json.Marshal(body)
			emit(assistant(fmt.Sprintf("m%dc", turn), map[string]any{"type": "text", "text": fmt.Sprintf("request=%v response=%s", resp["request_id"], seen)}))
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "done", "session_id": "sess-1",
				"duration_ms": 0, "num_turns": turn, "total_cost_usd": 0, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
		}
	}
}

// chatEvents は、Hub を購読して、届いた Event (JSON) を貯める。
type chatEvents struct {
	t      *testing.T
	sub    *chat.Subscription
	events []chat.Event
}

func newChatEvents(t *testing.T, s *chatSession) *chatEvents {
	t.Helper()
	sub, err := s.Chat.Hub.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	return &chatEvents{t: t, sub: sub, events: append([]chat.Event(nil), sub.Snapshot...)}
}

// waitFor は、pred に合う Event (すでに貯めた分から、次に来る分まで) を待って返す。
func (c *chatEvents) waitFor(what string, from int, pred func(chat.Event) bool) (chat.Event, int) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := from; ; i++ {
		for i >= len(c.events) {
			e, err := c.sub.Next(ctx)
			if err != nil {
				c.t.Fatalf("%s を待つ間に終わった: %v\nイベント: %s", what, err, c.dump())
			}
			c.events = append(c.events, e)
		}
		if pred(c.events[i]) {
			return c.events[i], i + 1
		}
	}
}

func (c *chatEvents) dump() string {
	var sb strings.Builder
	for _, e := range c.events {
		fmt.Fprintf(&sb, "\n  %d %s %.200s", e.Seq, e.Type, e.JSON)
	}
	return sb.String()
}

func typeIs(typ string) func(chat.Event) bool {
	return func(e chat.Event) bool { return e.Type == typ }
}

// requestIDOf は、permission.requested・resolved の Event の data.request_id。
func requestIDOf(t *testing.T, e chat.Event) string {
	t.Helper()
	var v struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(e.JSON, &v); err != nil {
		t.Fatal(err)
	}
	return v.Data.RequestID
}

func startChatFixture(t *testing.T, scene string) (*chatSession, *chatEvents, *bytes.Buffer) {
	t.Helper()
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)。CI の runner は非 root")
	}
	t.Setenv("HOME", f.home)
	t.Setenv("GORONATION_CLAUDE", f.exe)
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	s, err := startServeChatSession(ctx, f.stateDir(), "", "", "t", "t@e.invalid", f.repo, []string{scene}, &stderr)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Chat.Conv.Stop()
		s.Wait()
		cancel()
	})
	return s, newChatEvents(t, s), &stderr
}

// 結合テスト: 檻の中の (偽の) claude と、stream-json で、2 ターン + 権限のやり取りが通る。
// 指示を送る → 権限要求が Hub に出る → 承認 (allow_once) → claude が、許可した input を受け取る → 完了。2 ターン目は拒否 (reject_once)。
// 標準入出力は socketpair で、エージェントは dumpable=0 の複製から起動する (承認の経路に接続してよい状態)。
func TestChatSessionTwoTurnsWithPermissionsThroughCage(t *testing.T) {
	s, ev, _ := startChatFixture(t, "flow")
	conv := s.Chat.Conv

	if err := conv.Send("最初の指示"); err != nil {
		t.Fatal(err)
	}
	req1, at := ev.waitFor("1 つ目の権限要求", 0, typeIs("permission.requested"))
	id1 := requestIDOf(t, req1)
	if id1 != "perm-1" {
		t.Fatalf("request_id = %q", id1)
	}
	if conv.State() != chat.StateAwaitingPermission {
		t.Errorf("state = %v (awaiting_permission のはず)", conv.State())
	}
	if err := conv.Send("割り込み"); err == nil {
		t.Error("ターン中の Send が通った")
	}
	if err := conv.ResolveIn(conv.Generation(), id1, "allow_once"); err != nil {
		t.Fatal(err)
	}
	if err := conv.ResolveIn(conv.Generation(), id1, "allow_once"); err == nil {
		t.Error("同じ要求への 2 回目の承認が通った")
	}
	// 許可した input は、要求時に保持した値そのもの。claude が受け取った内容を、メッセージに書く。
	msg, at := ev.waitFor("承認の結果のメッセージ", at, func(e chat.Event) bool {
		return e.Type == "message.text" && strings.Contains(string(e.JSON), "request=perm-1")
	})
	for _, want := range []string{`behavior`, `allow`, `turn 1`} {
		if !strings.Contains(string(msg.JSON), want) {
			t.Errorf("claude が受けた応答に %q が無い: %s", want, msg.JSON)
		}
	}
	_, at = ev.waitFor("1 ターン目の完了", at, typeIs("turn.completed"))
	if conv.State() != chat.StateIdle {
		t.Errorf("state = %v (idle のはず)", conv.State())
	}

	if err := conv.Send("2 つ目の指示"); err != nil {
		t.Fatal(err)
	}
	req2, at := ev.waitFor("2 つ目の権限要求", at, typeIs("permission.requested"))
	if err := conv.ResolveIn(conv.Generation(), requestIDOf(t, req2), "reject_once"); err != nil {
		t.Fatal(err)
	}
	msg2, at := ev.waitFor("拒否の結果のメッセージ", at, func(e chat.Event) bool {
		return e.Type == "message.text" && strings.Contains(string(e.JSON), "request=perm-2")
	})
	if !strings.Contains(string(msg2.JSON), "deny") {
		t.Errorf("拒否が claude に伝わっていない: %s", msg2.JSON)
	}
	ev.waitFor("2 ターン目の完了", at, typeIs("turn.completed"))

	// 手動の「終了」: 未決は無く、檻が止まり、Hub が終わる。
	conv.Stop()
	s.Wait()
	if err := conv.Send("終了後"); err == nil {
		t.Error("終了後の Send が通った")
	}
	// Event の型の列に、想定した要素がある (seq 順)。
	types := ""
	for _, e := range ev.events {
		types += e.Type + " "
	}
	for _, want := range []string{"turn.started", "permission.requested", "permission.resolved", "turn.completed"} {
		if !strings.Contains(types, want) {
			t.Errorf("イベントに %s が無い: %s", want, types)
		}
	}
}

// 終了 (Stop) で、未決の要求が全部失効し (by=policy・cancelled)、Hub が終了になり、以後の承認は 404 (ErrUnknownRequest)。
func TestChatSessionStopExpiresPendingThroughCage(t *testing.T) {
	s, ev, _ := startChatFixture(t, "flow")
	conv := s.Chat.Conv
	if err := conv.Send("指示"); err != nil {
		t.Fatal(err)
	}
	req, at := ev.waitFor("権限要求", 0, typeIs("permission.requested"))
	id := requestIDOf(t, req)
	conv.Stop()
	res, _ := ev.waitFor("失効", at, func(e chat.Event) bool {
		return e.Type == "permission.resolved" && requestIDOf(t, e) == id
	})
	if !strings.Contains(string(res.JSON), `"by":"policy"`) || !strings.Contains(string(res.JSON), "cancelled") {
		t.Errorf("失効の Event = %s", res.JSON)
	}
	s.Wait()
	if err := conv.ResolveIn(conv.Generation(), id, "allow_once"); err == nil {
		t.Error("失効した要求への承認が通った")
	}
	// Hub は終わっている (新しい購読者は、Snapshot の後、すぐ EOF)。
	sub, err := s.Chat.Hub.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if _, err := sub.Next(ctx); err != nil {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("Hub が終わらない: %v", err)
	}
}

// エージェントが入力を読んでも何も返さない (hold) ときの、Stop: 檻が止まり、Wait が戻る。
func TestChatSessionStopKillsCage(t *testing.T) {
	s, _, _ := startChatFixture(t, "hold")
	_ = s.Chat.Conv.Send("指示")
	s.Chat.Conv.Stop()
	done := make(chan int, 1)
	go func() { done <- s.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop で檻が止まらない")
	}
}

// fakeChatSpoof は、偽の claude の spoof 場面: 標準入出力を継承させない子 (spoof-child) に、エージェントと init への、偽のフレームの
// 書き込み・fd の奪取・ptrace・メモリの書き換えを試させ、その結果 (child:<試み> => <結果>) を、標準エラー出力に出す。
func fakeChatSpoof() int {
	child := exec.Command("/opt/claude/claude", "spoof-child", strconv.Itoa(os.Getpid()))
	child.Stdin = nil
	child.Stdout = os.Stderr
	if err := child.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "child-error="+err.Error())
	}
	return 0
}

// B3 のゲートの、本番の起動経路 (startServeChatSession → startChatSession) での確認。TestChatStdioHardenedBlocksForgedFrames は、テストが
// 自分で組み立てた cageConfig (読めない複製・NonDumpable) を確かめるので、startChatSession が、その 2 つを外しても (cfg.AgentExe の置き換え・
// cfg.NonDumpable = true の欠落) 落ちない。この経路で、檻の中の子が、エージェントと init の標準入出力に、偽のフレームを届けられないことを固定する。
func TestChatSessionBlocksForgedFramesThroughProductionPath(t *testing.T) {
	s, _, stderr := startChatFixture(t, "spoof")
	s.Wait()
	out := stderr.String()
	if !strings.Contains(out, "child:forged-writes => ") {
		t.Fatalf("子の、偽のフレームの書き込みの試みが、動いていない:\n%s", out)
	}
	res := childResults(strings.ReplaceAll(out, "[agent] ", ""))
	for _, k := range []string{"pidfd_getfd:agent:fd0", "pidfd_getfd:agent:fd1", "pidfd_getfd:pid1:fd0", "pidfd_getfd:pid1:fd1", "ptrace:agent", "ptrace:pid1", "mem-rw:agent", "mem-rw:pid1"} {
		v, ok := res[k]
		if !ok {
			t.Errorf("試みの結果が無い: %s\n%s", k, out)
			continue
		}
		if v == "STOLEN" || v == "ATTACHED" || v == "OPENED" {
			t.Errorf("本番の起動経路で、%s が通った (%s): 偽のフレームを、エージェントの標準入出力に届けられる\n%s", k, v, out)
		}
	}
	for k, v := range res {
		if strings.Contains(v, "OPENED") && (strings.Contains(k, ":agent:") || strings.Contains(k, ":pid1:")) {
			t.Errorf("本番の起動経路で、%s が開けた (%s)", k, v)
		}
	}
	if v := res["forged-writes"]; v != "0" {
		t.Errorf("偽のフレームを %s 回書けた (0 のはず)\n%s", v, out)
	}
	sub, err := s.Chat.Hub.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for _, e := range sub.Snapshot {
		if strings.Contains(string(e.JSON), forgedMarker) {
			t.Errorf("Hub に、偽のフレームが届いた: %s", e.JSON)
		}
	}
}

//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// 偽の opencode (golden の場面を再生する。fakeopencode_linux_test.go) を、実際の serve --chat の経路 (startServeChatSession → init --relay-control →
// landlock-exec → 偽の opencode。ホストは、状態の行・SSE・session の作成・HTTP の指示) で動かす。実物の opencode の無い CI で、承認・form・Stop まで通す。

// goldenOpencodeDir は、golden の場面の置き場 (非 root のテストバイナリを別のディレクトリから動かすときは、GORO_GOLDEN_DIR で指す)。
func goldenOpencodeDir() string {
	if d := os.Getenv("GORO_GOLDEN_DIR"); d != "" {
		return d
	}
	return "../../spec/testdata/golden/opencode-serve"
}

func startOpencodeFixture(t *testing.T, scene string) (*chatSession, *chatEvents, *bytes.Buffer) {
	t.Helper()
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)。CI の runner は非 root")
	}
	b, err := os.ReadFile(filepath.Join(goldenOpencodeDir(), scene, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "scene.ndjson"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.repo, "add", ".")
	f.git(t, f.repo, "commit", "-q", "-m", "scene")
	t.Setenv("HOME", f.home)
	t.Setenv("GORONATION_OPENCODE", f.exe)
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	s, err := startServeChatSession(ctx, f.stateDir(), "", "opencode", "t", "t@e.invalid", f.repo, nil, &stderr)
	if err != nil {
		cancel()
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		s.Chat.Conv.Stop()
		s.cancel() // 会話が先に終わっていても、檻を止める
		s.Wait()
		cancel()
	})
	return s, newChatEvents(t, s), &stderr
}

// fakeRequests は、偽の opencode が受けた要求 (REQ 行。標準エラー出力の転送の [agent] 付き) を、"METHOD path body" で返す。
func fakeRequests(stderr *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(stderr.String(), "\n") {
		if _, q, ok := strings.Cut(l, "REQ "); ok {
			if s, err := strconv.Unquote(q); err == nil {
				out = append(out, s)
			}
		}
	}
	return out
}

func waitRequests(t *testing.T, stderr *bytes.Buffer, n int) []string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if r := fakeRequests(stderr); len(r) >= n {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("要求が %d 個に届かない: %q\n%s", n, fakeRequests(stderr), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func originOf(t *testing.T, e chat.Event) string {
	t.Helper()
	var v struct {
		Origin *struct {
			ID string `json:"id"`
		} `json:"origin"`
	}
	if err := json.Unmarshal(e.JSON, &v); err != nil {
		t.Fatal(err)
	}
	if v.Origin == nil {
		return ""
	}
	return v.Origin.ID
}

// 場面 simple-text: 指示 → 返答 → 完了。session の作成の本文 (permissions の順序) と、prompt の要求が、偽の opencode に届く。
func TestOpencodeChatSimpleText(t *testing.T) {
	s, ev, stderr := startOpencodeFixture(t, "simple-text")
	if err := s.Chat.Conv.Send("Say hello."); err != nil {
		t.Fatal(err)
	}
	m, at := ev.waitFor("返答", 0, typeIs("message.text"))
	if !strings.Contains(string(m.JSON), "Hello from the fake provider.") {
		t.Fatalf("%s", m.JSON)
	}
	ev.waitFor("完了", at, typeIs("turn.completed"))
	reqs := waitRequests(t, stderr, 2)
	if reqs[0] != "POST /api/session "+opencodeSessionBody {
		t.Errorf("session の作成 = %q", reqs[0])
	}
	if reqs[1] != `POST /api/session/ses_1/prompt {"text":"Say hello."}` {
		t.Errorf("prompt = %q", reqs[1])
	}
	if strings.Contains(stderr.String(), "AUTH-BAD") {
		t.Error("init の付けた Authorization が、偽の opencode に拒否された")
	}
}

// 場面 tool-call-shell-allow: 承認の要求 → allow_once → 返答の要求が once で届く → tool の完了 → 完了。
func TestOpencodeChatPermissionAllow(t *testing.T) {
	s, ev, stderr := startOpencodeFixture(t, "tool-call-shell-allow")
	conv := s.Chat.Conv
	if err := conv.Send("Do it."); err != nil {
		t.Fatal(err)
	}
	req, at := ev.waitFor("承認の要求", 0, typeIs("permission.requested"))
	id := requestIDOf(t, req)
	if id != "per_1" {
		t.Fatalf("request_id = %q", id)
	}
	if err := conv.ResolveIn(conv.Generation(), id, "allow_once"); err != nil {
		t.Fatal(err)
	}
	if err := conv.ResolveIn(conv.Generation(), id, "allow_once"); err == nil {
		t.Error("同じ要求への 2 回目の承認が通った")
	}
	res, at := ev.waitFor("決着", at, typeIs("permission.resolved"))
	if !strings.Contains(string(res.JSON), `"by":"human"`) {
		t.Errorf("%s", res.JSON)
	}
	up, at := ev.waitFor("tool の完了", at, func(e chat.Event) bool {
		return e.Type == "tool.update" && strings.Contains(string(e.JSON), `"completed"`)
	})
	if !strings.Contains(string(up.JSON), "goro-hi") {
		t.Errorf("%s", up.JSON)
	}
	ev.waitFor("完了", at, typeIs("turn.completed"))
	reqs := waitRequests(t, stderr, 3)
	if reqs[2] != `POST /api/session/ses_1/permission/per_1/reply {"decision":"once"}` {
		t.Errorf("返答 = %q", reqs[2])
	}
}

// 場面 tool-call-shell-reject: reject_once は reject で届き、tool は失敗する。
func TestOpencodeChatPermissionReject(t *testing.T) {
	s, ev, stderr := startOpencodeFixture(t, "tool-call-shell-reject")
	conv := s.Chat.Conv
	if err := conv.Send("Do it."); err != nil {
		t.Fatal(err)
	}
	req, at := ev.waitFor("承認の要求", 0, typeIs("permission.requested"))
	if err := conv.ResolveIn(conv.Generation(), requestIDOf(t, req), "reject_once"); err != nil {
		t.Fatal(err)
	}
	ev.waitFor("完了", at, typeIs("turn.completed"))
	reqs := waitRequests(t, stderr, 3)
	if reqs[2] != `POST /api/session/ses_1/permission/per_1/reply {"decision":"reject"}` {
		t.Errorf("返答 = %q", reqs[2])
	}
}

// allow_always は、通らず、何も送られない (ADR 0021 決定 3)。承認は、そのまま残り、reject_once で畳める。
func TestOpencodeChatAllowAlwaysIsRefused(t *testing.T) {
	s, ev, stderr := startOpencodeFixture(t, "tool-call-shell-allow")
	conv := s.Chat.Conv
	if err := conv.Send("Do it."); err != nil {
		t.Fatal(err)
	}
	req, _ := ev.waitFor("承認の要求", 0, typeIs("permission.requested"))
	if err := conv.ResolveIn(conv.Generation(), requestIDOf(t, req), "allow_always"); err == nil {
		t.Fatal("allow_always が通った")
	}
	for _, r := range fakeRequests(stderr) {
		if strings.Contains(r, "/permission/") {
			t.Fatalf("返答が送られた: %q", r)
		}
	}
}

// 場面 question-rule-allow: permission.requested は出ず、form.requested が直接出る (session の permissions の question: allow が、環境変数の *: ask に
// 打ち消されない)。回答は、form の返答として届く。
func TestOpencodeChatQuestionForm(t *testing.T) {
	s, ev, stderr := startOpencodeFixture(t, "question-rule-allow")
	conv := s.Chat.Conv
	if err := conv.Send("Ask me."); err != nil {
		t.Fatal(err)
	}
	form, at := ev.waitFor("form の要求", 0, typeIs("form.requested"))
	for _, e := range ev.events {
		if e.Type == "permission.requested" {
			t.Fatalf("question に承認の段が出た: %s", e.JSON)
		}
	}
	formID := requestIDOf(t, form)
	var r chat.FormResolve
	if err := json.Unmarshal([]byte(`{"request_id":"`+formID+`","outcome":"answered","answer":{"q0":"Red"}}`), &r); err != nil {
		t.Fatal(err)
	}
	if err := conv.ResolveFormIn(conv.Generation(), formID, r); err != nil {
		t.Fatal(err)
	}
	ev.waitFor("完了", at, typeIs("turn.completed"))
	reqs := waitRequests(t, stderr, 3)
	if reqs[2] != `POST /api/session/ses_1/form/frm_1/reply {"answer":{"q0":"Red"}}` {
		t.Errorf("返答 = %q", reqs[2])
	}
}

// 場面 subagent: 子の session の要求は Origin つきで出て、返答は子の session の URL に届く。
func TestOpencodeChatSubagentOriginAndReplyPath(t *testing.T) {
	s, ev, stderr := startOpencodeFixture(t, "subagent")
	conv := s.Chat.Conv
	if err := conv.Send("Do it."); err != nil {
		t.Fatal(err)
	}
	req1, at := ev.waitFor("subagent の起動の承認", 0, typeIs("permission.requested"))
	if originOf(t, req1) != "" {
		t.Errorf("root の要求に Origin: %s", req1.JSON)
	}
	if err := conv.ResolveIn(conv.Generation(), requestIDOf(t, req1), "allow_once"); err != nil {
		t.Fatal(err)
	}
	req2, at := ev.waitFor("子の要求", at, typeIs("permission.requested"))
	if originOf(t, req2) != "ses_2" {
		t.Errorf("子の要求の Origin = %q: %s", originOf(t, req2), req2.JSON)
	}
	if err := conv.ResolveIn(conv.Generation(), requestIDOf(t, req2), "allow_once"); err != nil {
		t.Fatal(err)
	}
	ev.waitFor("完了", at, typeIs("turn.completed"))
	reqs := waitRequests(t, stderr, 4)
	if reqs[2] != `POST /api/session/ses_1/permission/per_1/reply {"decision":"once"}` || reqs[3] != `POST /api/session/ses_2/permission/per_2/reply {"decision":"once"}` {
		t.Errorf("返答 = %q", reqs[2:])
	}
}

// 手動の終了 (Stop): 承認の待ちの途中でも、檻が止まり、未決は失効し、後片付けが済む。
func TestOpencodeChatStopWhilePending(t *testing.T) {
	s, ev, _ := startOpencodeFixture(t, "tool-call-shell-allow")
	conv := s.Chat.Conv
	if err := conv.Send("Do it."); err != nil {
		t.Fatal(err)
	}
	ev.waitFor("承認の要求", 0, typeIs("permission.requested"))
	conv.Stop()
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop で、檻が終わらない")
	}
	if conv.State() != chat.StateClosed {
		t.Errorf("state = %v", conv.State())
	}
}

// 檻が先に死んでも (SSE が切れる)、会話が終わり、後片付けが済む。
func TestOpencodeChatCageDeathEndsConversation(t *testing.T) {
	s, ev, _ := startOpencodeFixture(t, "tool-call-shell-allow")
	if err := s.Chat.Conv.Send("Do it."); err != nil {
		t.Fatal(err)
	}
	ev.waitFor("承認の要求", 0, typeIs("permission.requested"))
	s.cancel() // 檻を殺す
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("檻が死んでも、会話が終わらない")
	}
}

// serve --chat の -- 以降の引数は、opencode では断る (clone の作成より前に)。
func TestOpencodeChatRefusesExtraArgs(t *testing.T) {
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では動かせない")
	}
	t.Setenv("HOME", f.home)
	t.Setenv("GORONATION_OPENCODE", f.exe)
	var stderr bytes.Buffer
	_, err := startServeChatSession(t.Context(), f.stateDir(), "", "opencode", "t", "t@e.invalid", f.repo, []string{"--port", "1"}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "引数を受けない") {
		t.Fatalf("err = %v", err)
	}
}

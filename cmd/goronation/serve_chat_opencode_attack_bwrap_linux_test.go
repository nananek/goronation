//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// 偽の opencode が敵対的に動いたとき (SSE・session の作成・root の食い違い)、起動は失敗側に倒れ、会話に何も渡らず、檻・egress の後片付けが済む。

// tryOpencodeChat は、場面と偽の opencode の動き (fake.mode) を指定して、serve --chat を起動する (失敗しうる)。
func tryOpencodeChat(t *testing.T, scene, mode string) (*chatSession, *bytes.Buffer, error) {
	t.Helper()
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)。CI の runner は非 root")
	}
	b, err := os.ReadFile(filepath.Join(goldenOpencodeDir(), scene, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"scene.ndjson": b, "fake.mode": []byte(mode)} {
		if err := os.WriteFile(filepath.Join(f.repo, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.git(t, f.repo, "add", ".")
	f.git(t, f.repo, "commit", "-q", "-m", "scene")
	t.Setenv("HOME", f.home)
	t.Setenv("GORONATION_OPENCODE", f.exe)
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	s, err := startServeChatSession(ctx, f.stateDir(), "", "opencode", "t", "t@e.invalid", f.repo, nil, &stderr)
	if err == nil {
		t.Cleanup(func() { s.Chat.Conv.Stop(); s.Wait() })
	}
	return s, &stderr, err
}

func shortenConnect(t *testing.T) {
	t.Helper()
	old := opencodeConnectTimeout
	opencodeConnectTimeout = 2 * time.Second
	t.Cleanup(func() { opencodeConnectTimeout = old })
}

// 作成の応答の id が、SSE の最初の root と違う: 起動は失敗し (ErrRootMismatch)、会話に何も渡らない。
func TestOpencodeChatRootMismatchFailsClosed(t *testing.T) {
	_, stderr, err := tryOpencodeChat(t, "tool-call-shell-allow", "badroot")
	if err != chat.ErrRootMismatch {
		t.Fatalf("err = %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr.String(), "一致しない") {
		t.Errorf("理由が出ていない: %s", stderr)
	}
}

// 最初のイベントが server.connected でない: 起動は失敗する。
func TestOpencodeChatRequiresServerConnected(t *testing.T) {
	shortenConnect(t)
	_, _, err := tryOpencodeChat(t, "simple-text", "noconnected")
	if err == nil || !strings.Contains(err.Error(), "server.connected") {
		t.Fatalf("err = %v", err)
	}
}

// session の作成が 5xx: 起動は失敗する。
func TestOpencodeChatSessionCreateFailure(t *testing.T) {
	_, _, err := tryOpencodeChat(t, "simple-text", "createfail")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}

// root の session.created が来ない (ほかのイベントが流れ続けるだけ): 期限で、起動は失敗する (ハングしない)。
func TestOpencodeChatRootNeverArrives(t *testing.T) {
	shortenConnect(t)
	start := time.Now()
	_, stderr, err := tryOpencodeChat(t, "simple-text", "noroot")
	if err == nil || !strings.Contains(err.Error(), "確かめられなかった") {
		t.Fatalf("err = %v\n%s", err, stderr)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("期限で止まらない")
	}
}

// root の前に来た偽の permission.asked は、会話に渡らない (root が一致してから、渡す)。承認の要求としては、出ない。
func TestOpencodeChatPreRootEventsAreDropped(t *testing.T) {
	s, stderr, err := tryOpencodeChat(t, "tool-call-shell-allow", "preroot")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	ev := newChatEvents(t, s)
	for _, e := range ev.events {
		if strings.Contains(string(e.JSON), "per_evil") {
			t.Fatalf("root の前の要求が渡った: %s", e.JSON)
		}
	}
	if err := s.Chat.Conv.Send("Do it."); err != nil {
		t.Fatal(err)
	}
	req, _ := ev.waitFor("承認の要求", 0, typeIs("permission.requested"))
	if requestIDOf(t, req) != "per_1" {
		t.Fatalf("request_id = %q", requestIDOf(t, req))
	}
}

// 子 (檻の中の opencode) が、ホストへの状態の行 ({"ready":true}) を偽造しても、起動は成功しない: 子の標準出力は init の pipe で、init の標準出力
// (ホストへの socketpair) には届かない。init は、起動の証明 ({"url":…}) が先に来なければ、失敗の行 ({"error":…}) を返す。
func TestOpencodeChatChildCannotForgeReadyLine(t *testing.T) {
	_, stderr, err := tryOpencodeChat(t, "simple-text", "forgeready")
	if err == nil {
		t.Fatalf("偽造した ready で、起動できた\n%s", stderr)
	}
	if strings.Contains(stderr.String(), "FORGE-PROC1-OPENED") {
		t.Errorf("子が、init の標準出力を開けた: %s", stderr)
	}
	if _, ok := err.(*errOpencodeInit); !ok {
		t.Errorf("init の失敗の行のはず: %v", err)
	}
}

// SSE の切断は、会話の終わり (再接続しない): 檻が止まり、後片付けが済む (ADR 0022 決定 5・0051 決定 6)。
func TestOpencodeChatSSEDisconnectEndsConversation(t *testing.T) {
	s, stderr, err := tryOpencodeChat(t, "tool-call-shell-allow", "ssecut")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("SSE が切れても、檻が止まらない\n%s", stderr)
	}
}

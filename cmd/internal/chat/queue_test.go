package chat

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v0 "github.com/nananek/goronation/spec/v0"
)

// gate は、Write を、開けるまで止める io.Writer (エージェントが入力を読まない状態の模擬)。
type gate struct {
	mu   sync.Mutex
	open chan struct{}
	buf  bytes.Buffer
	fail error
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

func (g *gate) Write(p []byte) (int, error) {
	<-g.open
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail != nil {
		return 0, g.fail
	}
	return g.buf.Write(p)
}

func (g *gate) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

func TestQueuedWriterOrderAndLimits(t *testing.T) {
	g := newGate()
	q := NewQueuedWriter(g, 3, 100, nil)
	// 書く goroutine が 1 行を取り出して止まるので、積めるのは maxLines 行 + 取り出し中の 1 行。
	var full error
	n := 0
	for i := 0; i < 10 && full == nil; i++ {
		full = q.WriteLine([]byte(fmt.Sprintf("l%d\n", i)))
		n++
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(full, ErrQueueFull) || n < 4 || n > 5 {
		t.Fatalf("満杯: n=%d err=%v", n, full)
	}
	if err := q.WriteLine(bytes.Repeat([]byte("x"), 101)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("バイト上限を超える 1 行: %v", err)
	}
	close(g.open)
	q.Close()
	q.Wait()
	if got := g.String(); !strings.HasPrefix(got, "l0\nl1\nl2\n") { // 順序どおりに、書き切る
		t.Fatalf("書いた内容 = %q", got)
	}
	if err := q.WriteLine([]byte("late\n")); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("閉じた後: %v", err)
	}
}

func TestQueuedWriterFailureClosesAndReports(t *testing.T) {
	g := newGate()
	g.fail = io.ErrClosedPipe
	close(g.open)
	failed := make(chan error, 1)
	q := NewQueuedWriter(g, 0, 0, func(err error) { failed <- err })
	if err := q.WriteLine([]byte("a\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("onFail = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onFail が呼ばれない")
	}
	q.Wait()
	if err := q.WriteLine([]byte("b\n")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("失敗の後の書き込み = %v (書き込みの失敗を返すはず)", err)
	}
}

func TestQueuedWriterDiscard(t *testing.T) {
	g := newGate()
	q := NewQueuedWriter(g, 0, 0, nil)
	for i := 0; i < 5; i++ {
		q.WriteLine([]byte("x\n"))
	}
	q.Discard()
	close(g.open)
	q.Wait()
	if n := strings.Count(g.String(), "x\n"); n > 1 { // 取り出し中の 1 行だけが、書かれうる
		t.Fatalf("Discard の後に %d 行が書かれた", n)
	}
}

func testSession(t *testing.T, in io.Writer) (*Session, *int, *sync.Mutex) {
	t.Helper()
	l, err := Agent("claude")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	stops := 0
	s := NewSession(SessionConfig{Launch: l, ID: "s", Input: in, OnStop: func() { mu.Lock(); stops++; mu.Unlock() }})
	return s, &stops, &mu
}

// 洪水 (L11): 入力を読んでいるエージェントに対しては、上限を超える要求の洪水 (要求ごとに自動拒否の 1 行) で、会話は終わらない。
func TestFloodDoesNotEndConversationWhenAgentReads(t *testing.T) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { // エージェントの入力を、読み続ける
		defer close(done)
		io.Copy(io.Discard, pr)
	}()
	s, stops, smu := testSession(t, pw)
	if err := s.Conv.Send("hi"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxPendingRequests; i++ {
		s.Conv.OnLine(reqFrame(fmt.Sprintf("p%d", i)))
	}
	for i := 0; i < 3000; i++ { // 上限を超える要求の洪水
		s.Conv.OnLine(reqFrame(fmt.Sprintf("f%d", i)))
	}
	if st := s.Conv.State(); st == StateClosed {
		t.Fatal("要求の洪水で、会話が終わった")
	}
	smu.Lock()
	if *stops != 0 {
		t.Fatalf("OnStop が %d 回", *stops)
	}
	smu.Unlock()
	if s.Conv.Pending() != MaxPendingRequests {
		t.Fatalf("Pending = %d", s.Conv.Pending())
	}
	s.Finish(0)
	pw.Close()
	<-done
}

// 洪水 (L11): エージェントが入力を読まないなら、キューが満杯になり、会話を終える (設計: 読まないエージェントは、止める)。OnStop は 1 回。
func TestFloodEndsConversationWhenAgentDoesNotRead(t *testing.T) {
	g := newGate() // 開けない: 何も書き切れない
	s, stops, smu := testSession(t, g)
	if err := s.Conv.Send("hi"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxPendingRequests; i++ {
		s.Conv.OnLine(reqFrame(fmt.Sprintf("p%d", i)))
	}
	for i := 0; i < 3*DefaultQueueLines && s.Conv.State() != StateClosed; i++ {
		s.Conv.OnLine(reqFrame(fmt.Sprintf("f%d", i)))
	}
	if s.Conv.State() != StateClosed {
		t.Fatal("読まないエージェントに、書き続けても、会話が終わらない")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		smu.Lock()
		n := *stops
		smu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("OnStop が %d 回 (1 回のはず)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.Conv.Pending() != 0 {
		t.Fatal("終わった会話に、未決が残っている")
	}
	close(g.open)
	s.Finish(1)
}

// 入力への書き込みの失敗 (エージェントが終わった・入力が壊れた) は、会話を終える。
func TestInputWriteFailureStopsConversation(t *testing.T) {
	g := newGate()
	g.fail = io.ErrClosedPipe
	close(g.open)
	s, stops, smu := testSession(t, g)
	_ = s.Conv.Send("hi") // 書けたことにして、キューの中で失敗する
	deadline := time.Now().Add(5 * time.Second)
	for {
		smu.Lock()
		n := *stops
		smu.Unlock()
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("OnStop が %d 回", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAgentLookup(t *testing.T) {
	if _, err := Agent("opencode"); err == nil {
		t.Error("opencode は、M1.5 では動かせない")
	}
	if _, err := Agent(""); err == nil {
		t.Error("空の名前")
	}
	l, _ := Agent("claude")
	a := l.Args()
	a[0] = "tampered"
	if l.Args()[0] != "-p" {
		t.Error("Args が、内部の slice を返している")
	}
	// フラグと値は、トークンの完全一致で見る (部分文字列だと、`--setting-sources user,project` も通る)。フラグは 1 回だけ。
	args := l.Args()
	for flag, want := range map[string]string{
		"--input-format":           "stream-json",
		"--output-format":          "stream-json",
		"--permission-prompt-tool": "stdio",
		"--permission-mode":        "default",
		"--setting-sources":        "user",
	} {
		n := 0
		for i, tok := range args {
			if tok != flag {
				continue
			}
			n++
			if i+1 >= len(args) || args[i+1] != want {
				t.Errorf("%s の値が %q でない: %q", flag, want, args)
			}
		}
		if n != 1 {
			t.Errorf("%s が %d 回 (1 回だけのはず): %q", flag, n, args)
		}
	}
}

// 世代 (L12): 別の起動 (世代) の画面からの応答は、未決の表に触れずに断る。
func TestResolveInStaleGeneration(t *testing.T) {
	e := newConv(t, HubConfig{})
	_ = e.c.Send("hi")
	_ = e.c.OnLine(reqFrame("r"))
	if g := e.c.Generation(); len(g) != 32 {
		t.Fatalf("世代 = %q", g)
	}
	if err := e.c.ResolveIn("other-launch", "r", v0.AllowOnce); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("古い世代の応答 = %v", err)
	}
	if e.c.Pending() != 1 || len(e.written()) != 1 { // 書いたのは、最初の prompt の 1 行だけ
		t.Fatalf("古い世代の応答が、状態を動かした: pending=%d writes=%d", e.c.Pending(), len(e.written()))
	}
	if err := e.c.ResolveIn(e.c.Generation(), "r", v0.AllowOnce); err != nil {
		t.Fatal(err)
	}
	other := newConv(t, HubConfig{})
	if other.c.Generation() == e.c.Generation() {
		t.Fatal("2 つの会話の世代が同じ")
	}
}

// 世代なしの承認の経路は、型で無い (L2): Conversation の公開のメソッドに、Resolve が無く、ResolveIn だけがある。
func TestResolveWithoutGenerationIsNotExported(t *testing.T) {
	typ := reflect.TypeOf(&Conversation{})
	if _, ok := typ.MethodByName("Resolve"); ok {
		t.Error("世代なしの Resolve が公開されている")
	}
	if _, ok := typ.MethodByName("ResolveIn"); !ok {
		t.Error("ResolveIn が無い")
	}
	e := newConv(t, HubConfig{})
	if err := e.c.ResolveIn("", "r", v0.AllowOnce); !errors.Is(err, ErrNoGeneration) {
		t.Errorf("世代が空 = %v", err)
	}
}

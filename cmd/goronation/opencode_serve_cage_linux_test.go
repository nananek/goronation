//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// 実物の opencode 2.x を、goronation init (--relay-control・landlock-exec 包み・seccomp の socket 許可リスト) の下の、実 bwrap の檻 (非 root) で動かす、
// 共通の道具 (TestRealOpencodeInCage と、serve の golden の採取 (capture_opencode_serve_linux_test.go) が使う)。本番と同じ経路: cageSpec → init
// (--relay-control・--relay-token-env・--landlock-connect・--relay-version-prefix) → landlock-exec → opencode。ホストは、control (socketpair) へ fd を送る
// relayClient 越しにだけ話す (トークンは init だけが持ち、ヘッダは許可リストを通る)。

// realCage は、動いている檻と、ホスト側の口。
type realCage struct {
	t        *testing.T
	pair     *stdioPair
	stderr   *syncBuffer // init・opencode の標準エラー
	status   *syncBuffer // init の標準出力 ({"ready":true})
	viaProxy *syncBuffer // 檻の egress (proxy) を通った要求の記録
	done     chan error  // 檻 (bwrap) の終了
	hostDir  string      // この檻の、ホスト側の作業ディレクトリ (記録に入ってはいけない path)
	mu       sync.Mutex
	calls    []recordedCall
}

// startRealCage は、src (opencode 2.x の実行ファイル) を、provider (偽の OpenAI 互換の provider。http://fake-provider.test/v1/ への要求が、egress の偽物を経由して届く) と
// 一緒に、檻の中で起動し、init の {"ready":true} まで待つ。後片付けは t.Cleanup。
func startRealCage(t *testing.T, src string, provider http.Handler) *realCage {
	c := newChatCageFixture(t, true)
	dir := filepath.Dir(c.cfg.RunDir)
	agentExe, err := unreadableExeCopy(filepath.Join(dir, "oc"), src) // 実運用と同じく、読めない複製から起動する (ADR 0012)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.Agent, c.cfg.AgentExe = opencodeProfile, agentExe

	fake := httptest.NewServer(provider)
	t.Cleanup(fake.Close)
	fakeAddr := strings.TrimPrefix(fake.URL, "http://")
	var viaProxy syncBuffer
	proxy := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viaProxy.Write([]byte(r.Method + " " + r.URL.String() + "\n"))
		if r.Method == http.MethodConnect || r.URL.Hostname() != "fake-provider.test" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		up, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+fakeAddr+r.URL.RequestURI(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		up.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(up)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if fl != nil {
					fl.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	})}
	ul, err := net.Listen("unix", filepath.Join(c.cfg.RunDir, proxySockName))
	if err != nil {
		t.Fatal(err)
	}
	go proxy.Serve(ul)
	t.Cleanup(func() { proxy.Close() })

	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   "fake/fake-model",
		"provider": map[string]any{"fake": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "Fake",
			"options": map[string]any{"baseURL": "http://fake-provider.test/v1", "apiKey": "x"},
			"models":  map[string]any{"fake-model": map[string]any{"name": "Fake Model"}},
		}},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(c.cfg.Work, "opencode.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c.cfg.RelayPort = port
	c.cfg.RelayTokenEnv, c.cfg.RelayVersionPrefix = "OPENCODE_PASSWORD", "opencode v2.0."
	c.cfg.Args = []string{"serve", "--stdio", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port)}

	pair, err := newStdioPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pair.Close)
	spec := cageSpec(c.cfg)
	spec.Stdin, spec.Stdout = pair.AgentIn, pair.AgentOut
	var stderr, status syncBuffer
	spec.Stderr = &stderr
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	cmd, err := bwrap.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	pair.AgentIn.Close()
	pair.AgentOut.Close()
	go io.Copy(&status, pair.Out)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		t.Logf("init の状態: %q\ninit・opencode の標準エラー:\n%s", status.String(), stderr.String())
	})

	waitRelayCond(t, "init の {\"ready\":true}", func() bool {
		select {
		case err := <-done:
			t.Fatalf("檻が先に終わった: %v\nstatus=%q\n%s", err, status.String(), stderr.String())
		default:
		}
		return strings.Contains(status.String(), "\n")
	})
	if got := status.String(); got != "{\"ready\":true}\n" {
		t.Fatalf("起動の結果 = %q\n%s", got, stderr.String())
	}
	return &realCage{t: t, hostDir: dir, pair: pair, stderr: &stderr, status: &status, viaProxy: &viaProxy, done: done}
}

// recordedCall は、ホスト → サーバーの 1 回の要求と応答 (SSE 以外)。
type recordedCall struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Body     string `json:"body,omitempty"`
	Status   int    `json:"status"`
	Response string `json:"response,omitempty"`
}

// call は、relayClient 越しの 1 回の要求 (本文があれば Content-Type: application/json。許可リストのヘッダだけ)。記録 (calls) にも残す。
func (c *realCage) call(method, path, body string) (int, string) {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, "http://opencode.invalid"+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := newRelayClient(c.pair.In).HTTPClient()
	hc.Timeout = 60 * time.Second
	resp, err := hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	c.mu.Lock()
	c.calls = append(c.calls, recordedCall{method, path, body, resp.StatusCode, string(out)})
	c.mu.Unlock()
	return resp.StatusCode, string(out)
}

// recordedCalls は、これまでの call の記録 (順序どおり)。
func (c *realCage) recordedCalls() []recordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedCall(nil), c.calls...)
}

// sseEvent は、SSE の 1 イベント (id・event の行と、data: の行)。
type sseEvent struct {
	ID, Event, Data string
}

// sseStream は、GET /api/event の、到着順の全イベント。
type sseStream struct {
	mu   sync.Mutex
	evs  []sseEvent
	done bool
}

// openSSE は、GET /api/event を、許可リストのヘッダ (Accept・Cache-Control) で開く。
func (c *realCage) openSSE() *sseStream {
	c.t.Helper()
	req, _ := http.NewRequest("GET", "http://opencode.invalid/api/event", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := newRelayClient(c.pair.In).HTTPClient().Do(req)
	if err != nil || resp.StatusCode != 200 {
		c.t.Fatalf("GET /api/event: %v %v", err, resp)
	}
	s := &sseStream{}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var cur sseEvent
		flush := func() {
			if cur != (sseEvent{}) {
				s.mu.Lock()
				s.evs = append(s.evs, cur)
				s.mu.Unlock()
				cur = sseEvent{}
			}
		}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				flush()
			case strings.HasPrefix(line, "id: "):
				cur.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				cur.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				if cur.Data != "" {
					cur.Data += "\n"
				}
				cur.Data += line[6:]
			}
		}
		flush()
		s.mu.Lock()
		s.done = true
		s.mu.Unlock()
	}()
	return s
}

// snapshot は、いままでのイベントのコピー。
func (s *sseStream) snapshot() []sseEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sseEvent(nil), s.evs...)
}

// eventType は、data の JSON の "type" (JSON でなければ "")。
func (e sseEvent) eventType() string {
	var m struct {
		Type string `json:"type"`
	}
	json.Unmarshal([]byte(e.Data), &m)
	return m.Type
}

// waitFor は、from 番目以降のイベントで pred を満たす最初のものの番号を返す (期限までに来なければ t.Fatalf。SSE が閉じても)。
func (s *sseStream) waitFor(t *testing.T, what string, from int, timeout time.Duration, pred func(sseEvent) bool) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		evs, done := s.evs, s.done
		for i := from; i < len(evs); i++ {
			if pred(evs[i]) {
				s.mu.Unlock()
				return i
			}
		}
		s.mu.Unlock()
		if done {
			t.Fatalf("%s を待つ間に、SSE が閉じた", what)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s が、%v 以内に来ない", what, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForType は、type が typ のイベントを待つ。
func (s *sseStream) waitForType(t *testing.T, typ string, from int, timeout time.Duration) int {
	t.Helper()
	return s.waitFor(t, "イベント "+typ, from, timeout, func(e sseEvent) bool { return e.eventType() == typ })
}

// stop は、control を閉じる (init が子を止めて、檻が終わる)。終わるまで待つ。
func (c *realCage) stop() {
	c.t.Helper()
	c.pair.In.Close()
	select {
	case <-c.done:
	case <-time.After(30 * time.Second):
		c.t.Error("control を閉じても、檻が終わらない")
	}
}

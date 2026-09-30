//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/sandbox/bwrap"
	"github.com/nananek/goronation/tools/fakeproviders/openai"
)

// 実物の opencode 2.x を、goronation init (--relay-control・landlock-exec 包み・seccomp の socket 許可リスト) の下の、実 bwrap の檻 (非 root) で動かす
// 確認 (ADR 0020・0029・0030)。opencode の実体が要るので、GORO_REAL_OPENCODE=<opencode の実行ファイルの path> のときだけ動く (通常は skip)。
// 確かめること: 版の検査 (opencode v2.0.)・起動の証明・{"ready":true}・許可リストのヘッダだけで API・SSE が通ること・承認つきの 1 ターン
// (偽の provider・http の proxy 経由)。子が landlock-exec の下 (no_new_privs・seccomp) にあることは、この test では確かめない (opencode は自分の status を出さない)。
// その機構は、偽の子での bwrap の test (relay_bwrap_linux_test.go の SANDBOX の検査) が確かめる。
//
// 偽の provider は、host の loopback の http サーバー。檻からは、egress の UDS (run dir の proxy.sock) に立てた、host 側の偽の egress (http の proxy) が、
// http://fake-provider.test/ を、その偽の provider に中継する。
func TestRealOpencodeInCage(t *testing.T) {
	src := os.Getenv("GORO_REAL_OPENCODE")
	if src == "" {
		t.Skip("GORO_REAL_OPENCODE (opencode 2.x の実行ファイルの path) が無い")
	}
	c := newChatCageFixture(t, true)
	dir := filepath.Dir(c.cfg.RunDir)
	agentExe, err := unreadableExeCopy(filepath.Join(dir, "oc"), src) // 実運用と同じく、読めない複製から起動する (ADR 0012)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.Agent, c.cfg.AgentExe = opencodeProfile, agentExe

	// 偽の provider と、偽の egress (http の proxy)。
	// 要求に tools があれば会話の本体 (1 回目は shell の tool 呼び出し、tool の結果のあとは "done")。無ければ、opencode が別に出す題名の生成。
	llm := openai.NewServer(
		openai.Step{ToolCalls: []openai.ToolCall{{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{"command":"echo goro-hi; ls -l /proc/self/fd; echo ---TCP; cat /proc/net/tcp","description":"say hi"}`)}}},
		openai.Step{Content: "done"},
	)
	titles := openai.NewServer(openai.Step{Content: "Say hi"})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"tools"`)) {
			llm.ServeHTTP(w, r)
		} else {
			titles.ServeHTTP(w, r)
		}
	}))
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

	hc := newRelayClient(pair.In).HTTPClient()
	hc.Timeout = 60 * time.Second
	call := func(method, path, body string) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, "http://opencode.invalid"+path, rd)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	if st, body := call("GET", "/api/info", ""); st != 200 || !strings.Contains(body, `"version":"2.0.`) {
		t.Fatalf("GET /api/info = %d %s", st, body)
	}

	// SSE (長い接続。許可リストのヘッダ: Accept・Cache-Control)。
	sseReq, _ := http.NewRequest("GET", "http://opencode.invalid/api/event", nil)
	sseReq.Header.Set("Accept", "text/event-stream")
	sseReq.Header.Set("Cache-Control", "no-cache")
	sseClient := newRelayClient(pair.In).HTTPClient()
	sseResp, err := sseClient.Do(sseReq)
	if err != nil || sseResp.StatusCode != 200 {
		t.Fatalf("GET /api/event: %v %v", err, sseResp)
	}
	events := make(chan map[string]any, 256)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(sseResp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				if json.Unmarshal([]byte(data), &m) == nil {
					t.Logf("SSE: %.300s", data)
					events <- m
				}
			}
		}
	}()

	st, body := call("POST", "/api/session", `{"permissions":[{"action":"*","resource":"*","effect":"ask"}]}`)
	var sess struct {
		ID   string `json:"id"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(body), &sess)
	if sess.ID == "" {
		sess.ID = sess.Data.ID
	}
	if st != 200 || sess.ID == "" {
		t.Fatalf("POST /api/session = %d %s", st, body)
	}
	if st, body := call("POST", "/api/session/"+sess.ID+"/prompt", `{"text":"run echo goro-hi"}`); st/100 != 2 {
		t.Fatalf("prompt = %d %s", st, body)
	}

	// 承認を待つ (permission.asked) → 1 回だけ許可 → 終わる (偽の provider の 2 回目の応答 "done")。
	var replied bool
	deadline := time.After(90 * time.Second)
	for !replied {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("SSE が閉じた\n%s", stderr.String())
			}
			if ev["type"] == "permission.asked" {
				data, _ := ev["data"].(map[string]any)
				rid, _ := data["id"].(string)
				if st, body := call("POST", "/api/session/"+sess.ID+"/permission/"+rid+"/reply", `{"decision":"once"}`); st/100 != 2 {
					t.Fatalf("reply = %d %s", st, body)
				}
				replied = true
			}
		case <-deadline:
			t.Fatalf("permission.asked が来ない\nproxy: %s\n%s", viaProxy.String(), stderr.String())
		}
	}
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("SSE が閉じた (承認の後)")
			}
			if ev["type"] == "session.execution.succeeded" { // tool の結果を渡した 2 回目の provider の応答 ("done") まで終わった
				goto finished
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("承認の後、終わらない\nproxy: %s", viaProxy.String())
		}
	}
finished:
	if !strings.Contains(viaProxy.String(), "fake-provider.test") {
		t.Errorf("provider への通信が、proxy (egress) を経由していない: %q", viaProxy.String())
	}
	reqs := llm.Requests()
	if n := len(reqs); n < 2 {
		t.Errorf("provider への要求 = %d 個 (tool の結果を含む 2 回目が要る)", n)
	} else {
		var m struct {
			Messages []struct{ Role, Content string }
		}
		json.Unmarshal(reqs[len(reqs)-1].Body, &m)
		for _, x := range m.Messages {
			if x.Role == "tool" {
				checkToolFds(t, x.Content)
			}
		}
	}
	pair.In.Close() // control を閉じる: 檻が終わる
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Error("control を閉じても、檻が終わらない")
	}
}

// checkToolFds は、tool (shell) が `ls -l /proc/self/fd; echo ---TCP; cat /proc/net/tcp` で出した結果を調べる (ADR 0029 の限界 L-1・L-2 の前提): tool が、
// opencode の待ち受けのソケット (TCP) も、init の子の標準出力の pipe (起動の証明の書き側) も、継承していないこと。
func checkToolFds(t *testing.T, out string) {
	t.Helper()
	fds, tcp, ok := strings.Cut(out, "---TCP")
	if !ok || !strings.Contains(out, "goro-hi") {
		t.Errorf("tool の結果が想定と違う: %q", out)
		return
	}
	t.Logf("tool の fd:\n%s", fds)
	if strings.Contains(fds, "pipe:[") {
		t.Errorf("tool が pipe を継承している (起動の証明の書き側かもしれない):\n%s", fds)
	}
	tcpInodes := map[string]bool{}
	for _, line := range strings.Split(tcp, "\n")[1:] {
		if f := strings.Fields(line); len(f) >= 10 {
			tcpInodes[f[9]] = true
		}
	}
	if len(tcpInodes) == 0 {
		t.Errorf("/proc/net/tcp に、待ち受けが 1 つも無い (読めていない)")
	}
	for _, m := range regexp.MustCompile(`socket:\[(\d+)\]`).FindAllStringSubmatch(fds, -1) {
		if tcpInodes[m[1]] {
			t.Errorf("tool が TCP のソケット (inode %s) を継承している (待ち受けの fd の継承。ADR 0029 の L-2)", m[1])
		}
	}
}

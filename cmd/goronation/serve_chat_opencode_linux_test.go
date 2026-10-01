//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// clientTo は、URL のホストを無視して、srv に繋ぐ http.Client (本番の relayClient.HTTPClient の代わり)。
func clientTo(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
		DisableKeepAlives: true,
	}}
}

type pipeCloser struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (p *pipeCloser) Close() error { p.once.Do(func() { close(p.closed) }); return nil }

func TestReadReadyLine(t *testing.T) {
	long := strings.Repeat("a", opencodeReadyMax+10)
	for _, c := range []struct {
		name, in string
		want     string // "" なら成功。それ以外は、error に含まれる文字列
		initErr  bool
	}{
		{"ready", "{\"ready\":true}\n", "", false},
		{"error", "{\"error\":\"ポートが使われている\"}\n", "ポートが使われている", true},
		{"error の制御文字", "{\"error\":\"a\\u001b[2Jb\"}\n", "a?[2Jb", true},
		{"ready false", "{\"ready\":false}\n", "成功でも失敗でもない", false},
		{"両方", "{\"ready\":true,\"error\":\"x\"}\n", "成功でも失敗でもない", false},
		{"未知の欄", "{\"ready\":true,\"x\":1}\n", "読めない", false},
		{"JSON でない", "ready\n", "読めない", false},
		{"空の object", "{}\n", "成功でも失敗でもない", false},
		{"巨大", long + "\n", "超える", false},
		{"EOF", "", "状態の行を出さずに終わった", false},
		{"改行なしで EOF", "{\"ready\":true}", "状態の行を出さずに終わった", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := readReadyLine(&pipeCloser{Reader: strings.NewReader(c.in), closed: make(chan struct{})}, time.Second)
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if _, ok := err.(*errOpencodeInit); ok != c.initErr {
				t.Fatalf("init の error = %v, want %v", ok, c.initErr)
			}
		})
	}
}

// 時間切れは、失敗で、読み元を閉じる。
func TestReadReadyLineTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	pc := &pipeCloser{Reader: pr, closed: make(chan struct{})}
	err := readReadyLine(pc, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "以内") {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-pc.closed:
	default:
		t.Fatal("読み元を閉じていない")
	}
}

func sseServer(t *testing.T, body string, hold bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Cache-Control") != "no-cache" {
			http.Error(w, "bad", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
		w.(http.Flusher).Flush()
		if hold {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenEventStream(t *testing.T) {
	for _, c := range []struct {
		name, body string
		hold       bool
		want       string
	}{
		{"ok", "data: {\"type\":\"server.connected\",\"data\":{}}\n\n", false, ""},
		{"コメントの後", ": hi\n\nevent: x\ndata: {\"type\":\"server.connected\"}\n\n", false, ""},
		{"最初が違う", "data: {\"type\":\"session.created\"}\n\n", false, "server.connected でない"},
		{"JSON でない", "data: hello\n\n", false, "server.connected でない"},
		{"閉じる", "", false, "最初のイベントを読めない"},
		{"来ない", ": ping\n\n", true, "以内"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := sseServer(t, c.body, c.hold)
			resp, sr, err := openEventStream(t.Context(), clientTo(srv), 200*time.Millisecond)
			if c.want == "" {
				if err != nil || sr == nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestOpenEventStreamNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) }))
	defer srv.Close()
	if _, _, err := openEventStream(t.Context(), clientTo(srv), time.Second); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

// permissions の本文は、順序つきで固定: すべて ask → question だけ allow (あとの規則が勝つ。ADR 0043)。
func TestSessionBodyOrder(t *testing.T) {
	var b struct {
		Permissions []struct{ Action, Resource, Effect string } `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(opencodeSessionBody), &b); err != nil {
		t.Fatal(err)
	}
	want := []struct{ Action, Resource, Effect string }{{"*", "*", "ask"}, {"question", "*", "allow"}}
	if fmt.Sprint(b.Permissions) != fmt.Sprint(want) {
		t.Fatalf("permissions = %v", b.Permissions)
	}
}

func TestCreateSession(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string // 空なら成功 (id)
	}{
		{"ok", 200, `{"data":{"id":"ses_1"}}`, "ses_1"},
		{"201", 201, `{"data":{"id":"ses_9"}}`, "ses_9"},
		{"500", 500, `{"data":{"id":"ses_1"}}`, ""},
		{"id が無い", 200, `{"data":{}}`, ""},
		{"id の形が不正", 200, `{"data":{"id":"../x"}}`, ""},
		{"JSON でない", 200, `ok`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got struct{ ct, body string }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got.ct, got.body = r.Header.Get("Content-Type"), string(b)
				if r.Method != "POST" || r.URL.Path != "/api/session" {
					t.Errorf("%s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			}))
			defer srv.Close()
			id, err := createSession(t.Context(), clientTo(srv), time.Second)
			if got.ct != "application/json" || got.body != opencodeSessionBody {
				t.Errorf("要求 = %q %q", got.ct, got.body)
			}
			if c.want == "" {
				if err == nil {
					t.Fatalf("成功した: %q", id)
				}
				return
			}
			if err != nil || id != c.want {
				t.Fatalf("id=%q err=%v", id, err)
			}
		})
	}
}

func recordingServer(t *testing.T, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, fmt.Sprintf("%s %s %s ct=%q", r.Method, r.URL.Path, b, r.Header.Get("Content-Type")))
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestHTTPWriterExecutesAllowedRequests(t *testing.T) {
	srv, got := recordingServer(t, 204)
	w := &httpWriter{ctx: t.Context(), hc: clientTo(srv), timeout: time.Second}
	for _, line := range []string{
		`{"method":"POST","path":"/api/session/ses_1/prompt","body":{"text":"hi"}}` + "\n",
		`{"method":"POST","path":"/api/session/ses_1/permission/per_1/reply","body":{"decision":"once"}}` + "\n",
		`{"method":"DELETE","path":"/api/session/ses_1/form/frm_1","body":null}` + "\n",
		`{"method":"POST","path":"/api/session/ses_1/interrupt","body":null}` + "\n",
	} {
		if n, err := w.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("%q: n=%d err=%v", line, n, err)
		}
	}
	want := []string{
		`POST /api/session/ses_1/prompt {"text":"hi"} ct="application/json"`,
		`POST /api/session/ses_1/permission/per_1/reply {"decision":"once"} ct="application/json"`,
		`DELETE /api/session/ses_1/form/frm_1  ct=""`,
		`POST /api/session/ses_1/interrupt  ct=""`,
	}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Fatalf("got = %q", *got)
	}
}

// アダプタを通さない行を直接書いても、許可外の要求は、実行されない。
func TestHTTPWriterRefusesDisallowedRequests(t *testing.T) {
	srv, got := recordingServer(t, 204)
	w := &httpWriter{ctx: t.Context(), hc: clientTo(srv), timeout: time.Second}
	for _, line := range []string{
		`{"method":"GET","path":"/api/session/ses_1/prompt","body":{"text":"x"}}` + "\n",
		`{"method":"POST","path":"/api/other","body":{}}` + "\n",
		`{"method":"POST","path":"/api/session/../prompt","body":{}}` + "\n",
		`{"method":"POST","path":"/api/session/ses_1/prompt","body":{},"x":1}` + "\n",
		"not json\n",
	} {
		if _, err := w.Write([]byte(line)); err == nil {
			t.Errorf("通った: %q", line)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("実行された: %q", *got)
	}
}

// 2xx 以外は error (会話が終わる)。
func TestHTTPWriterNon2xxFails(t *testing.T) {
	for _, status := range []int{400, 401, 404, 500, 503} {
		srv, _ := recordingServer(t, status)
		w := &httpWriter{ctx: t.Context(), hc: clientTo(srv), timeout: time.Second}
		if _, err := w.Write([]byte(`{"method":"POST","path":"/api/session/ses_1/interrupt","body":null}` + "\n")); err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

// 返答が遅いと、期限で error になる (ハングしない)。
func TestHTTPWriterTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	w := &httpWriter{ctx: t.Context(), hc: clientTo(srv), timeout: 100 * time.Millisecond}
	start := time.Now()
	if _, err := w.Write([]byte(`{"method":"POST","path":"/api/session/ses_1/interrupt","body":null}` + "\n")); err == nil {
		t.Fatal("通った")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("期限で止まらない")
	}
}

// 2xx 以外の書き込みは、QueuedWriter 越しに、会話を終える (承認の返答の失敗が、承認済みのように続かない)。
func TestHTTPWriterFailureStopsConversation(t *testing.T) {
	srv, _ := recordingServer(t, 500)
	l, err := chat.Agent("opencode")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	s := chat.NewSession(chat.SessionConfig{
		Launch: l, ID: "s", Input: &httpWriter{ctx: t.Context(), hc: clientTo(srv), timeout: time.Second},
		OnStop: func() { close(stopped) },
	})
	s.ReadSSE(chat.NewSSEReader(strings.NewReader("data: {\"type\":\"session.created\",\"data\":{\"sessionID\":\"ses_1\"}}\n\n")), "ses_1", nil)
	if err := s.Conv.Send("hi"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("500 でも、会話が終わらない")
	}
}

func TestCheckAgentArgs(t *testing.T) {
	oc, _ := chat.Agent("opencode")
	cl, _ := chat.Agent("claude")
	if err := checkAgentArgs(oc, nil); err != nil {
		t.Error(err)
	}
	for _, a := range [][]string{{"--port", "1"}, {"--hostname=0.0.0.0"}, {""}} {
		if err := checkAgentArgs(oc, a); err == nil {
			t.Errorf("opencode が %q を受けた", a)
		}
	}
	if err := checkAgentArgs(cl, []string{"flow"}); err != nil {
		t.Errorf("claude は受ける: %v", err)
	}
}

// profile の値を、本番と試験が共有する (opencode の値)。
func TestOpencodeProfileRelayValues(t *testing.T) {
	if opencodeProfile.relayTokenEnv != "OPENCODE_PASSWORD" || opencodeProfile.relayVersionPrefix != "opencode v2.0." {
		t.Fatalf("%q %q", opencodeProfile.relayTokenEnv, opencodeProfile.relayVersionPrefix)
	}
	if claudeProfile.relayTokenEnv != "" {
		t.Error("claude は HTTP の transport を使わない")
	}
}

// OPENCODE_CONFIG_CONTENT は、MCP の有無によらず、permission も持つ 1 つの JSON (後勝ちで片方が消えない)。
func TestOpencodeServeEnvMergesMCPAndPermission(t *testing.T) {
	for _, servers := range [][]mcpServerDef{nil, {{Name: "goronation", Command: "/opt/goronation/goronation", Args: []string{"mcp"}}}} {
		env := opencodeServeEnv(servers)
		if len(env) != 1 || env[0].Key != "OPENCODE_CONFIG_CONTENT" {
			t.Fatalf("env = %v", env)
		}
		var cfg struct {
			Permission map[string]string          `json:"permission"`
			MCP        map[string]json.RawMessage `json:"mcp"`
		}
		if err := json.Unmarshal([]byte(env[0].Value), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Permission["*"] != "ask" || len(cfg.Permission) != 1 {
			t.Errorf("permission = %v", cfg.Permission)
		}
		if (len(servers) > 0) != (len(cfg.MCP) > 0) {
			t.Errorf("mcp = %v (servers=%d)", cfg.MCP, len(servers))
		}
	}
}

// cageSpec は、HTTP の transport (RelayPort) のとき、環境変数を 1 つにまとめた値を使い、OPENCODE_CONFIG_CONTENT は 1 回だけ渡す。
func TestCageSpecServeEnvIsSingle(t *testing.T) {
	c := cageConfig{Agent: opencodeProfile, RelayPort: 4567, RelayTokenEnv: "OPENCODE_PASSWORD",
		MCPServers: []mcpServerDef{{Name: "goronation", Command: "/opt/goronation/goronation", Args: []string{"mcp"}}}}
	n := 0
	var val string
	for _, e := range cageSpec(c).Env {
		if e.Key == "OPENCODE_CONFIG_CONTENT" {
			n++
			val = e.Value
		}
	}
	if n != 1 || !strings.Contains(val, `"permission":{"*":"ask"}`) || !strings.Contains(val, `"mcp"`) {
		t.Fatalf("n=%d val=%s", n, val)
	}
	// RelayPort が無い (goronation run) の opencode は、これまでどおり (permission は足さない)。
	c.RelayPort = 0
	for _, e := range cageSpec(c).Env {
		if e.Key == "OPENCODE_CONFIG_CONTENT" && strings.Contains(e.Value, "permission") {
			t.Fatalf("run に permission が入った: %s", e.Value)
		}
	}
}

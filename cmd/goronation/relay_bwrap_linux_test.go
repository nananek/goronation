//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// 逆方向中継 (ADR 0019・0028) の、bwrap の檻での確認。檻の中: goronation init (PID 1・dumpable=0・--relay-control) が、子 (偽の opencode) の
// 127.0.0.1:PORT へ、ホストが socketpair + SCM_RIGHTS で送る HTTP の要求を、Authorization を付け直して中継する。

// relayCage は、起動した檻と、その control・記録 (標準エラー出力)。
type relayCage struct {
	t      *testing.T
	cmd    *bwrap.Cmd
	pair   *stdioPair
	rc     *relayClient
	stderr *syncBuffer
	status *syncBuffer // init の標準出力 (起動の結果の 1 行。ADR 0030)
	port   int
	done   chan error
}

// startRelayCage は、偽の opencode (relay-upstream) を子にした、--relay-control の檻を起こす。spoof なら、偽の opencode が奪取の試みもする。
func startRelayCage(t *testing.T, spoof bool) *relayCage {
	return startRelayCageHardened(t, spoof, true)
}

// startRelayCageHardened は、hardened=false なら NonDumpable を立てずに (RelayPort だけで) 檻を起こす: cageSpec が強制することの確認。
func startRelayCageHardened(t *testing.T, spoof, hardened bool) *relayCage {
	t.Helper()
	c := newChatCageFixture(t, hardened)
	l, err := net.Listen("tcp", "127.0.0.1:0") // 空きポートを、ホストで探す (檻の netns は別だが、衝突しない値にする)
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c.cfg.RelayPort = port
	c.cfg.RelayTokenEnv, c.cfg.RelayVersionPrefix = "OPENCODE_PASSWORD", "opencode v2.0."
	c.cfg.Args = []string{"relay-upstream", strconv.Itoa(port)}
	if spoof {
		c.cfg.Args = append(c.cfg.Args, "spoof")
	}
	pair, err := newStdioPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pair.Close)
	spec := cageSpec(c.cfg)
	spec.Stdin, spec.Stdout = pair.AgentIn, pair.AgentOut // 標準入力 = control。標準出力は、init が使わない (読み捨て)
	rc := &relayCage{t: t, pair: pair, stderr: &syncBuffer{}, status: &syncBuffer{}, port: port, done: make(chan error, 1)}
	spec.Stderr = rc.stderr
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	cmd, err := bwrap.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	pair.AgentIn.Close()
	pair.AgentOut.Close()
	go io.Copy(rc.status, pair.Out)
	rc.cmd = cmd
	rc.rc = newRelayClient(pair.In)
	go func() { rc.done <- cmd.Wait() }()
	waitRelayCond(t, "偽の opencode の起動", func() bool { return strings.Contains(rc.stderr.String(), "UPSTREAM-READY") })
	// init が、起動の証明を確かめて、{"ready":true} を出すまで待つ (それまで、要求は受け付けられない。ADR 0029・0030)。
	waitRelayCond(t, "init の {\"ready\":true}", func() bool { return rc.status.String() == "{\"ready\":true}\n" })
	return rc
}

func waitRelayCond(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s を待ってもできない", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reqs は、偽の opencode が記録した要求 (REQ 行) を、元の文字列にして返す。
func (r *relayCage) reqs() []string {
	var out []string
	for _, l := range strings.Split(r.stderr.String(), "\n") {
		if q, ok := strings.CutPrefix(l, "REQ "); ok {
			if s, err := strconv.Unquote(q); err == nil {
				out = append(out, s)
			}
		}
	}
	return out
}

var basicTokenRE = regexp.MustCompile(`(?m)^Authorization: Basic ([A-Za-z0-9+/=]+)\r$`)

// 通し: ホストの http.Client の要求が、control 経由で檻の中の init に届き、偽の opencode に、init のトークンの認証だけを付けて中継される。
// クライアントが偽造した Authorization・Host は届かない。SSE は流れ、ホストが閉じると、上流も閉じる。control を閉じると、檻 (init と子) が終わる。
func TestRelayInBwrapEndToEnd(t *testing.T) {
	rc := startRelayCage(t, false)
	hc := rc.rc.HTTPClient()
	base := "http://opencode.invalid"
	var tokens []string
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", base+"/api/session/ses_"+strconv.Itoa(i)+"/prompt", strings.NewReader(`{"text":"hi"}`))
		req.Header.Set("Authorization", "Bearer forged-"+strconv.Itoa(i))
		req.Header.Set("Proxy-Authorization", "Basic forged")
		req.Host = "evil.example"
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "ok" {
			t.Fatalf("status=%d body=%q\nstderr:\n%s", resp.StatusCode, b, rc.stderr.String())
		}
	}
	reqs := rc.reqs()
	if len(reqs) != 3 {
		t.Fatalf("偽の opencode が受けた要求 = %d 個\n%s", len(reqs), rc.stderr.String())
	}
	for _, r := range reqs {
		if strings.Contains(r, "forged") || strings.Contains(r, "evil.example") || strings.Count(r, "Authorization:") != 1 {
			t.Errorf("偽造したヘッダが届いた・Authorization が 1 つでない:\n%s", r)
		}
		if !strings.Contains(r, "\r\nHost: 127.0.0.1:"+strconv.Itoa(rc.port)+"\r\n") || !strings.HasSuffix(r, `{"text":"hi"}`) {
			t.Errorf("Host・本文が想定と違う:\n%s", r)
		}
		m := basicTokenRE.FindStringSubmatch(r)
		if m == nil {
			t.Fatalf("Basic 認証が無い:\n%s", r)
		}
		dec, err := base64.StdEncoding.DecodeString(m[1])
		user, tok, _ := strings.Cut(string(dec), ":")
		if err != nil || user != "opencode" || len(tok) != 43 {
			t.Errorf("認証の中身が想定と違う: user=%q トークンの長さ=%d", user, len(tok))
		}
		tokens = append(tokens, tok)
	}
	if tokens[0] != tokens[1] || tokens[1] != tokens[2] {
		t.Errorf("同じ init の中で、トークンが変わった: %q", tokens)
	}
	// トークンは、子の環境変数 OPENCODE_PASSWORD と一致し、argv には無い。子は landlock-exec の下で起動された (no_new_privs・seccomp)。
	lines := strings.Split(rc.stderr.String(), "\n")
	rec := func(prefix string) string {
		for _, l := range lines {
			if v, ok := strings.CutPrefix(l, prefix+" "); ok {
				return v
			}
		}
		return ""
	}
	if env := rec("TOKEN-ENV"); env != tokens[0] || strings.Contains(rec("ARGV"), tokens[0]) {
		t.Errorf("子の環境のトークン = %q (Authorization の %q と一致し、argv に無いはず)", env, tokens[0])
	}
	if rec("SANDBOX NoNewPrivs:") != "1" || rec("SANDBOX Seccomp:") != "2" {
		t.Errorf("子が landlock-exec の下で起動されていない: NoNewPrivs=%q Seccomp=%q", rec("SANDBOX NoNewPrivs:"), rec("SANDBOX Seccomp:"))
	}

	// SSE: 流れてくる。ホストが閉じると、上流の接続も閉じる。
	resp, err := hc.Get(base + "/api/event")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for n := 0; n < 3; {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE: %v", err)
		}
		if strings.HasPrefix(line, "data: event-") {
			n++
		}
	}
	resp.Body.Close()
	waitRelayCond(t, "ホストが閉じたあとの上流の接続の close", func() bool { return strings.Contains(rc.stderr.String(), "SSE-CLOSED") })

	// control を閉じる (ホストが終わる・会話の終了): init が子を止めて、檻が終わる。
	rc.pair.In.Close()
	select {
	case <-rc.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("control を閉じても、檻が終わらない\n%s", rc.stderr.String())
	}
}

// B3 のゲート (ADR 0012・0019): control (init の標準入力の socket) も、要求ごとの fd も、同じ uid の檻の中の子は奪えない・乗っ取れない。
// 子 (spoof-child) は、自分以外のすべてのプロセス (init = PID 1 を含む) に対し、/proc/<pid>/fd/{0,1} の開き直し・connect・pidfd_getfd・ptrace・
// /proc/<pid>/mem を試す。init (pid1) に対するものは、すべて断られる。偽の opencode (子の親) は、unix domain socket の fd を持たない (control を継承していない)。
// ptrace_scope=1 の環境 (CI の runner) では、yama だけでも断られるので、dumpable=0 が効いていることは、ここでは区別できない: 変異で確かめる (ADR 0012 の運用の約束)。
func TestRelayInBwrapControlNotStealable(t *testing.T) {
	for _, hardened := range []bool{true, false} {
		name := "NonDumpable つき"
		if !hardened {
			name = "RelayPort だけ (cageSpec が強制する)"
		}
		t.Run(name, func(t *testing.T) { relayControlNotStealable(t, hardened) })
	}
}

func relayControlNotStealable(t *testing.T, hardened bool) {
	rc := startRelayCageHardened(t, true, hardened)
	waitRelayCond(t, "奪取の試みの完了", func() bool { return strings.Contains(rc.stderr.String(), "agent-done") })
	out := rc.stderr.String()
	r := childResults(out)
	// PID 1 は goronation init (bwrap が PID 1 のまま、control を dumpable で持っていない)。scope=1 (CI) でも区別できる。
	if r["pid1-init"] != "true" {
		t.Errorf("PID 1 が goronation init (--relay-control) ではない: %q\n%s", r["pid1-init"], out)
	}
	if r["unix-sockets-in-agent"] != "0" {
		t.Errorf("偽の opencode が unix domain socket の fd を持つ (control を継承した): %q", r["unix-sockets-in-agent"])
	}
	want := []string{"open-w:pid1:fd0", "open-w:pid1:fd1", "open-rw:pid1:fd0", "connect:pid1:fd0", "pidfd_getfd:pid1:fd0", "pidfd_getfd:pid1:fd1", "ptrace:pid1", "mem-rw:pid1"}
	for _, k := range want {
		v, ok := r[k]
		if !ok {
			t.Errorf("試みの結果が無い: %s\n%s", k, out)
			continue
		}
		switch v {
		case "OPENED", "STOLEN", "ATTACHED", "CONNECTED":
			t.Errorf("%s => %s (init の control を奪えてはいけない)", k, v)
		}
	}
	// 奪取の試みの後も、control 経由の要求は通る (init が乗っ取られていない・壊れていない)。
	resp, err := rc.rc.HTTPClient().Get("http://opencode.invalid/api/info")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" {
		t.Errorf("奪取の試みの後の要求 = %q", b)
	}
}

// 版の検査 (ADR 0018・0030): 檻の中で、--version の出力が prefix で始まらなければ、子を起動せず、標準出力に {"error":…} を出して、檻が終わる。
func TestRelayInBwrapVersionMismatch(t *testing.T) {
	c := newChatCageFixture(t, true)
	c.cfg.RelayPort = 4567
	c.cfg.RelayTokenEnv, c.cfg.RelayVersionPrefix = "OPENCODE_PASSWORD", "opencode v9."
	c.cfg.Args = []string{"relay-upstream", "4567"}
	pair, err := newStdioPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pair.Close)
	spec := cageSpec(c.cfg)
	spec.Stdin, spec.Stdout = pair.AgentIn, pair.AgentOut
	var stderr, status syncBuffer
	spec.Stderr = &stderr
	cmd, err := bwrap.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	pair.AgentIn.Close()
	pair.AgentOut.Close()
	go io.Copy(&status, pair.Out)
	if err := cmd.Wait(); err == nil {
		t.Error("版が違うのに、檻が正常に終わった")
	}
	waitRelayCond(t, "{\"error\":…} の行", func() bool { return strings.HasPrefix(status.String(), `{"error":"`) })
	if strings.Contains(stderr.String(), "UPSTREAM-READY") {
		t.Error("版が違うのに、子が起動した")
	}
}

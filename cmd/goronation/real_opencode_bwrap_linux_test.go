//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

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
	// 要求に tools があれば会話の本体 (1 回目は shell の tool 呼び出し、tool の結果のあとは "done")。無ければ、opencode が別に出す題名の生成。
	llm := openai.NewServer(
		openai.Step{ToolCalls: []openai.ToolCall{{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{"command":"echo goro-hi; ls -l /proc/self/fd; echo ---TCP; cat /proc/net/tcp","description":"say hi"}`)}}},
		openai.Step{Content: "done"},
	)
	titles := openai.NewServer(openai.Step{Content: "Say hi"})
	c := startRealCage(t, src, splitProvider(llm, titles))

	if st, body := c.call("GET", "/api/info", ""); st != 200 || !strings.Contains(body, `"version":"2.0.`) {
		t.Fatalf("GET /api/info = %d %s", st, body)
	}

	// SSE (長い接続。許可リストのヘッダ: Accept・Cache-Control)。
	sse := c.openSSE()

	st, body := c.call("POST", "/api/session", `{"permissions":[{"action":"*","resource":"*","effect":"ask"}]}`)
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
	if st, body := c.call("POST", "/api/session/"+sess.ID+"/prompt", `{"text":"run echo goro-hi"}`); st/100 != 2 {
		t.Fatalf("prompt = %d %s", st, body)
	}

	// 承認を待つ (permission.asked) → 1 回だけ許可 → 終わる (偽の provider の 2 回目の応答 "done")。
	i := sse.waitForType(t, "permission.asked", 0, 90*time.Second)
	var asked struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(sse.snapshot()[i].Data), &asked)
	if st, body := c.call("POST", "/api/session/"+sess.ID+"/permission/"+asked.Data.ID+"/reply", `{"decision":"once"}`); st/100 != 2 {
		t.Fatalf("reply = %d %s", st, body)
	}
	sse.waitForType(t, "session.execution.succeeded", i, 60*time.Second) // tool の結果を渡した 2 回目の provider の応答 ("done") まで終わった

	if !strings.Contains(c.viaProxy.String(), "fake-provider.test") {
		t.Errorf("provider への通信が、proxy (egress) を経由していない: %q", c.viaProxy.String())
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
	c.stop()
}

// splitProvider は、要求に tools があれば main (会話の本体)、無ければ titles (opencode が別に出す題名の生成) に振る、偽の provider。
func splitProvider(main, titles http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"tools"`)) {
			main.ServeHTTP(w, r)
		} else {
			titles.ServeHTTP(w, r)
		}
	})
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

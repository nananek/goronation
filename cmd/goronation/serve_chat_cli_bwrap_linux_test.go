package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

// root では、serve --chat は、何も作らずに、理由を言って断る (L3)。
func TestServeChatRefusesRootClearly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root でだけ確かめる")
	}
	var out, errb bytes.Buffer
	dir := t.TempDir()
	if code := runServe([]string{"--chat", "--repo", dir, "--state-dir", filepath.Join(dir, "state")}, &out, &errb); code != exitUsage {
		t.Errorf("終了コード = %d (%d のはず)", code, exitUsage)
	}
	if !strings.Contains(errb.String(), "root では動かせない") {
		t.Errorf("理由が出ない: %q", errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); err == nil {
		t.Error("断る前に、状態のディレクトリを作った")
	}
}

func TestChatSocketPathIsSiblingOfTerm(t *testing.T) {
	if got, want := chatSocketPath("/s", "default", "id1"), "/s/groups/default/sessions/id1/chat.sock"; got != want {
		t.Errorf("chatSocketPath = %q", got)
	}
}

// startServeChatFixture は、goronation serve --chat (偽の claude の場面 scene) を、実プロセスで起こす。
func startServeChatFixture(t *testing.T, scene string) (*liveRun, *http.Client) {
	t.Helper()
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)。CI の runner は非 root")
	}
	sock := filepath.Join(shortDir(t), "chat.sock")
	r := f.start(t, "serve", "--chat", "--state-dir", f.stateDir(), "--socket", sock, "--repo", f.repo, "--", scene)
	t.Cleanup(func() {
		syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
		r.wait()
	})
	waitStderrMatch(t, r, serveListenRE)
	return r, udsClient(sock)
}

// serve --chat の全体 (実プロセス・実際の bwrap の檻・UDS): hello の世代 → 指示 → 権限要求 → 承認 (世代つき) → 完了 → 終了。
func TestServeChatEndToEnd(t *testing.T) {
	r, cli := startServeChatFixture(t, "flow")
	resp, err := cli.Get("http://serve.invalid/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 4<<20)
	read := func(pred func(event, data string) bool) string {
		t.Helper()
		event := ""
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				event = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "data: "):
				if d := strings.TrimPrefix(l, "data: "); pred(event, d) {
					return d
				}
			case l == "":
				event = ""
			}
		}
		t.Fatalf("目的のイベントの前に、ストリームが終わった\nstderr:\n%s", r.stderr.String())
		return ""
	}
	hello := read(func(e, _ string) bool { return e == "hello" })
	var h struct {
		FirstSeq   uint64 `json:"first_seq"`
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal([]byte(hello), &h); err != nil || len(h.Generation) != 32 {
		t.Fatalf("hello = %s", hello)
	}
	do := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", "http://serve.invalid"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, b := do("/message", `{"text":"hi"}`); code != 200 {
		t.Fatalf("message: %d %s", code, b)
	}
	req := read(func(e, d string) bool { return e == "" && strings.Contains(d, `"type":"permission.requested"`) })
	var rv struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(req), &rv)
	perm := func(gen string) (int, string) {
		b, _ := json.Marshal(map[string]string{"generation": gen, "request_id": rv.Data.RequestID, "outcome": "allow_once"})
		return do("/permission", string(b))
	}
	if code, _ := perm("0123456789abcdef0123456789abcdef"); code != 409 {
		t.Errorf("別の世代 = %d (409 のはず)", code)
	}
	if code, b := perm(h.Generation); code != 200 {
		t.Fatalf("承認: %d %s", code, b)
	}
	read(func(e, d string) bool { return e == "" && strings.Contains(d, `"type":"turn.completed"`) })
	if code, _ := do("/stop", ``); code != 200 {
		t.Errorf("stop = %d", code)
	}
	if end := read(func(e, _ string) bool { return e == "end" }); !strings.HasPrefix(end, `{"exit":`) {
		t.Errorf("end = %s", end)
	}
}

// エージェントが、serve の ready 行に見える行を、標準エラー出力に出しても、serve --chat の標準エラー出力の中で、ready 行として一致するのは、
// serve 自身の 1 行だけ (goronation web の spawnServe が、セッション ID を偽られない。PR④ の Blocking C を、配線の後も固定する)。
func TestServeChatStderrCannotForgeReadyLine(t *testing.T) {
	r, _ := startServeChatFixture(t, "stderr-spoof")
	waitStderrMatch(t, r, regexp.MustCompile(`\[agent\] .*abcdef`))
	n := 0
	for _, l := range strings.Split(r.stderr.String(), "\n") {
		if serveReadyRE.MatchString(strings.TrimRight(l, "\r")) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ready 行に一致する行が %d 本 (1 本のはず):\n%s", n, r.stderr.String())
	}
}

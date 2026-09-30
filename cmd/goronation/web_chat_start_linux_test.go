package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// POST /api/chat/start のテスト (bwrap は要らない): goronation serve の代わりに、起動の引数を記録し、ready 行を出す、偽の実行ファイルを使う。

// fakeServeExe は、goronation serve の代わりの実行ファイル (シェルスクリプト): 引数を log に 1 行で足し、delay 秒待ち、ready 行 (session=id) を標準エラー出力に出す。
func fakeServeExe(t *testing.T, log, delay, id string) string {
	t.Helper()
	dir := shortDir(t)
	exe := filepath.Join(dir, "goronation")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nsleep " + delay + "\necho \"goronation serve: /x/chat.sock で待ち受けている (session=" + id + ")\" >&2\nexec sleep 5\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func startReposDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := shortDir(t)
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func logLines(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestWebChatStartSpawnsServeWithChatFlag(t *testing.T) {
	withTimeout(t, 60*time.Second)
	repos := startReposDir(t, "proj")
	log := filepath.Join(shortDir(t), "args.log")
	cw := newChatWebFull(t, fastRelay(), webReadTimeout, webWriteTimeout, repos, fakeServeExe(t, log, "0", chatTestID))
	code, body := cw.req("POST", "/api/chat/start", `{"repo":"proj"}`, nil)
	if code != 200 {
		t.Fatalf("start = %d %s", code, body)
	}
	var r struct{ ID string }
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.ID != chatTestID {
		t.Fatalf("応答 = %s", body)
	}
	lines := logLines(t, log)
	if len(lines) != 1 {
		t.Fatalf("serve の起動が %d 回 (1 回のはず): %v", len(lines), lines)
	}
	want := "serve --state-dir " + cw.stateDir + " --chat --repo " + filepath.Join(repos, "proj")
	if lines[0] != want {
		t.Errorf("引数 = %q (%q のはず)", lines[0], want)
	}
}

// 認証・関門・不正な repo は、serve を起こさない (クレジットを使わない)。
func TestWebChatStartGatesNeverSpawn(t *testing.T) {
	withTimeout(t, 60*time.Second)
	repos := startReposDir(t, "proj")
	outside := shortDir(t)
	if err := os.Symlink(outside, filepath.Join(repos, "escape")); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(shortDir(t), "args.log")
	cw := newChatWebFull(t, fastRelay(), webReadTimeout, webWriteTimeout, repos, fakeServeExe(t, log, "0", chatTestID))
	anon := &http.Client{}
	if code, _ := cw.reqWith(anon, "POST", "/api/chat/start", `{"repo":"proj"}`, nil); code != 401 {
		t.Errorf("無認証 = %d (401 のはず)", code)
	}
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"Origin なし", map[string]string{"Origin": ""}, 403},
		{"別の Origin", map[string]string{"Origin": "http://evil.example"}, 403},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"text/plain", map[string]string{"Content-Type": "text/plain"}, 415},
		{"Content-Type なし", map[string]string{"Content-Type": ""}, 415},
	} {
		if code, b := cw.req("POST", "/api/chat/start", `{"repo":"proj"}`, tc.headers); code != tc.want {
			t.Errorf("%s: %d %.80s (%d のはず)", tc.name, code, b, tc.want)
		}
	}
	for _, body := range []string{`{"repo":""}`, `{"repo":".."}`, `{"repo":"../x"}`, `{"repo":"a/b"}`, `{"repo":"nope"}`, `{"repo":"escape"}`, `{}`, `{"repo":"proj","extra":1}`, `not json`, `{"repo":1}`} {
		if code, _ := cw.req("POST", "/api/chat/start", body, nil); code != 400 && code != 404 {
			t.Errorf("%s = %d (400 か 404 のはず)", body, code)
		}
	}
	if resp, err := cw.client.Get(cw.web.URL + "/api/chat/start"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("GET = %d (405 のはず)", resp.StatusCode)
		}
	}
	if lines := logLines(t, log); len(lines) != 0 {
		t.Errorf("serve が起動された: %v", lines)
	}
	// --repos-dir なしでも、serve は起こさない。
	cw2 := newChatWebFull(t, fastRelay(), webReadTimeout, webWriteTimeout, "", fakeServeExe(t, log, "0", chatTestID))
	if code, _ := cw2.req("POST", "/api/chat/start", `{"repo":"proj"}`, nil); code != 404 {
		t.Errorf("--repos-dir なし = %d (404 のはず)", code)
	}
	if lines := logLines(t, log); len(lines) != 0 {
		t.Errorf("serve が起動された: %v", lines)
	}
}

// 二重クリック・二重送信: 同じ repo の起動待ちは 1 つだけ (chat が 2 つ起動しない)。別の repo は、並行に起動できる。終われば、また起動できる。
func TestWebChatStartDoubleClickStartsOnce(t *testing.T) {
	withTimeout(t, 90*time.Second)
	repos := startReposDir(t, "proj", "other")
	log := filepath.Join(shortDir(t), "args.log")
	cw := newChatWebFull(t, fastRelay(), webReadTimeout, webWriteTimeout, repos, fakeServeExe(t, log, "2", chatTestID))
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = cw.req("POST", "/api/chat/start", `{"repo":"proj"}`, nil)
		}()
	}
	time.Sleep(700 * time.Millisecond)
	if code, _ := cw.req("POST", "/api/chat/start", `{"repo":"other"}`, nil); code != 200 { // 別の repo は通る
		t.Errorf("別の repo = %d", code)
	}
	wg.Wait()
	n200, n409 := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			n200++
		case 409:
			n409++
		default:
			t.Errorf("想定外の status %d", c)
		}
	}
	if n200 != 1 || n409 != 4 {
		t.Errorf("200 が %d 個・409 が %d 個 (1 と 4 のはず)", n200, n409)
	}
	if lines := logLines(t, log); len(lines) != 2 { // proj 1 回 + other 1 回
		t.Errorf("serve の起動が %d 回 (2 回のはず): %v", len(lines), lines)
	}
	if code, _ := cw.req("POST", "/api/chat/start", `{"repo":"proj"}`, nil); code != 200 { // 終わった後は、また起動できる
		t.Errorf("終わった後の再起動 = %d", code)
	}
}

// 起動待ちの数の上限 (超過は 503)。
func TestWebChatStartLimit(t *testing.T) {
	withTimeout(t, 90*time.Second)
	names := []string{"a", "b", "c", "d", "e"}
	repos := startReposDir(t, names...)
	log := filepath.Join(shortDir(t), "args.log")
	cw := newChatWebFull(t, fastRelay(), webReadTimeout, webWriteTimeout, repos, fakeServeExe(t, log, "2", chatTestID))
	var wg sync.WaitGroup
	codes := make([]int, len(names))
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 100 * time.Millisecond)
			codes[i], _ = cw.req("POST", "/api/chat/start", `{"repo":"`+n+`"}`, nil)
		}()
	}
	wg.Wait()
	n503 := 0
	for _, c := range codes {
		if c == 503 {
			n503++
		}
	}
	if n503 != 1 {
		t.Errorf("status = %v (503 が 1 つのはず)", codes)
	}
}

// serve が起動に失敗したら (ready 行が出ない)、502。root では動かせない旨は、分かる文言で返す。
func TestWebChatStartFailureLabels(t *testing.T) {
	withTimeout(t, 60*time.Second)
	repos := startReposDir(t, "proj")
	dir := shortDir(t)
	exe := filepath.Join(dir, "goronation")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'goronation serve: 何かが壊れた' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cw := newChatWebFull(t, fastRelay(), webReadTimeout, webWriteTimeout, repos, exe)
	code, body := cw.req("POST", "/api/chat/start", `{"repo":"proj"}`, nil)
	if code != 502 || strings.Contains(body, "壊れた") {
		t.Errorf("失敗 = %d %s (502 で、serve の出力を返さない)", code, body)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'goronation serve: goronation serve --chat は root では動かせない (x)' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, body = cw.req("POST", "/api/chat/start", `{"repo":"proj"}`, nil)
	if code != 500 || !strings.Contains(body, "root では動かせない") {
		t.Errorf("root = %d %s", code, body)
	}
}

// state-dir が長くて chat.sock を作れないなら、serve を起こす前に断る (セッションを作ってから失敗しない)。
func TestWebChatStartLongStateDir(t *testing.T) {
	withTimeout(t, 60*time.Second)
	repos := startReposDir(t, "proj")
	log := filepath.Join(shortDir(t), "args.log")
	cw := newChatWebFull(t, fastRelay(), webReadTimeout, webWriteTimeout, repos, fakeServeExe(t, log, "0", chatTestID))
	long := filepath.Join(cw.stateDir, strings.Repeat("d", 60))
	// web の stateDir を、長いものに差し替える代わりに、長い stateDir の web を作る。
	cw2 := newChatWebLongState(t, repos, fakeServeExe(t, log, "0", chatTestID), long)
	if code, b := cw2.req("POST", "/api/chat/start", `{"repo":"proj"}`, nil); code != 500 || !strings.Contains(b, "短く") {
		t.Errorf("長い state-dir = %d %s", code, b)
	}
	if lines := logLines(t, log); len(lines) != 0 {
		t.Errorf("serve が起動された: %v", lines)
	}
}

// chat.sock の有無が、セッション一覧の chat に出る。
func TestChatSockExists(t *testing.T) {
	dir := shortDir(t)
	if chatSockExists(dir, chatTestID) {
		t.Error("無いのに、ある")
	}
	sock := chatSocketPath(dir, defaultGroup, chatTestID)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil { // ソケットでない通常のファイルは、ソケットとみなさない
		t.Fatal(err)
	}
	if chatSockExists(dir, chatTestID) {
		t.Error("通常のファイルを、ソケットとみなした")
	}
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !chatSockExists(dir, chatTestID) {
		t.Error("ソケットがあるのに、無い")
	}
}

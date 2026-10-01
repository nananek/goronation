package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 実プロセス (実際の bwrap の檻・UDS) の、耐久ログの寿命: --socket を動かしても DB はセッションのディレクトリ・hello は durable=true・エージェントが終わった後も
// after で再接続できる (resumed=true で、続きと end)・SIGTERM の終了で、DB・-wal・-shm が消える。
func TestServeChatEventLogLifecycle(t *testing.T) {
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)。CI の runner は非 root")
	}
	sockDir := shortDir(t)
	sock := filepath.Join(sockDir, "chat.sock")
	r := f.start(t, "serve", "--chat", "--state-dir", f.stateDir(), "--socket", sock, "--repo", f.repo, "--", "flow")
	exited := false
	t.Cleanup(func() {
		if !exited {
			syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
			r.wait()
		}
	})
	m := waitStderrMatch(t, r, regexp.MustCompile(`goronation serve: .+ で待ち受けている \(session=(\S+)\)`))
	sessDir := sessionDirOf(f.stateDir(), m[1])
	if _, err := os.Stat(filepath.Join(sessDir, "events.db")); err != nil {
		t.Fatalf("DB が、セッションのディレクトリ (--socket と無関係) に無い: %v", err)
	}
	if left := dbFilesIn(t, sockDir); len(left) != 0 {
		t.Fatalf("--socket のディレクトリに、DB を作った: %v", left)
	}
	cli := udsClient(sock)

	get := func(query string) (*bufio.Scanner, func()) {
		resp, err := cli.Get("http://serve.invalid/events" + query)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET /events%s: %v %v", query, resp, err)
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 4<<20)
		return sc, func() { resp.Body.Close() }
	}
	type frame struct{ event, id, data string }
	next := func(sc *bufio.Scanner) (f frame, ok bool) {
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				f.event = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "id: "):
				f.id = strings.TrimPrefix(l, "id: ")
			case strings.HasPrefix(l, "data: "):
				f.data = strings.TrimPrefix(l, "data: ")
			case l == "":
				return f, true
			}
		}
		return f, false
	}
	sc, closeSSE := get("")
	hf, _ := next(sc)
	var h struct {
		Generation string `json:"generation"`
		Durable    bool   `json:"durable"`
		Resumed    bool   `json:"resumed"`
	}
	if err := json.Unmarshal([]byte(hf.data), &h); err != nil || hf.event != "hello" || !h.Durable || h.Resumed || len(h.Generation) != 32 {
		t.Fatalf("hello = %+v %v", hf, err)
	}
	do := func(path, body string) {
		req, _ := http.NewRequest("POST", "http://serve.invalid"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := cli.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("POST %s: %v %v\nstderr:\n%s", path, resp, err, r.stderr.String())
		}
		resp.Body.Close()
	}
	do("/message", `{"text":"hi"}`)
	var lastSeq string
	var reqID string
	for {
		f, ok := next(sc)
		if !ok {
			t.Fatalf("permission.requested の前に終わった\nstderr:\n%s", r.stderr.String())
		}
		if f.id != "" {
			lastSeq = f.id
		}
		if strings.Contains(f.data, `"type":"permission.requested"`) {
			var rv struct {
				Data struct {
					RequestID string `json:"request_id"`
				} `json:"data"`
			}
			json.Unmarshal([]byte(f.data), &rv)
			reqID = rv.Data.RequestID
			break
		}
	}
	// 切断 → after で繋ぎ直す (まだ未決の権限要求は、画面がすでに持っている)。
	closeSSE()
	do("/permission", fmt.Sprintf(`{"generation":%q,"request_id":%q,"outcome":"allow_once"}`, h.Generation, reqID))
	do("/stop", ``)
	// エージェントが終わるのを待つ (end が届く場に、別の接続で待つ): 終わった後も、serve は動いていて、after で再接続できる。
	var sawEnd bool
	for i := 0; i < 200 && !sawEnd; i++ {
		sc2, close2 := get("")
		next(sc2) // hello
		for {
			f, ok := next(sc2)
			if !ok {
				break
			}
			if f.event == "end" {
				sawEnd = true
				break
			}
		}
		close2()
		if !sawEnd {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !sawEnd {
		t.Fatalf("エージェントが終わらない\nstderr:\n%s", r.stderr.String())
	}
	sc3, close3 := get(fmt.Sprintf("?after=%s&generation=%s", lastSeq, h.Generation))
	defer close3()
	hf3, _ := next(sc3)
	var h3 struct {
		Resumed bool `json:"resumed"`
		Durable bool `json:"durable"`
	}
	json.Unmarshal([]byte(hf3.data), &h3)
	if !h3.Resumed || !h3.Durable {
		t.Fatalf("終了後の再接続の hello = %s", hf3.data)
	}
	prev := lastSeq
	gotEnd := false
	for {
		f, ok := next(sc3)
		if !ok {
			break
		}
		if f.event == "end" {
			gotEnd = true
			break
		}
		var a, b uint64
		fmt.Sscan(prev, &a)
		fmt.Sscan(f.id, &b)
		if b <= a {
			t.Fatalf("seq が戻った・重複した: %s の次に %s", prev, f.id)
		}
		prev = f.id
	}
	if !gotEnd || prev == lastSeq {
		t.Fatalf("after の続き (id: が lastSeq=%s より後) と end が届かない (end=%v last=%s)", lastSeq, gotEnd, prev)
	}

	// SIGTERM: 正常な終了で、DB・-wal・-shm が消える。
	syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
	r.wait()
	exited = true
	if left := dbFilesIn(t, sessDir); len(left) != 0 {
		t.Fatalf("終了のあとに、DB が残った: %v", left)
	}
}

// kill -9 の残りは、次の serve の起動の掃除で消える (生きている別のセッションは、消さない)。
func TestServeChatEventLogSweepsKilledServeRemains(t *testing.T) {
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)。CI の runner は非 root")
	}
	readyRE := regexp.MustCompile(`goronation serve: .+ で待ち受けている \(session=(\S+)\)`)
	start := func() (*liveRun, string) {
		sock := filepath.Join(shortDir(t), "chat.sock")
		r := f.start(t, "serve", "--chat", "--state-dir", f.stateDir(), "--socket", sock, "--repo", f.repo, "--", "hold")
		t.Cleanup(func() {
			syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
			r.wait()
		})
		return r, waitStderrMatch(t, r, readyRE)[1]
	}
	first, id1 := start()
	dir1 := sessionDirOf(f.stateDir(), id1)
	if len(dbFilesIn(t, dir1)) == 0 {
		t.Fatal("DB が無い")
	}
	syscall.Kill(first.cmd.Process.Pid, syscall.SIGKILL) // 掃除も削除もできない死に方
	first.wait()
	if len(dbFilesIn(t, dir1)) == 0 {
		t.Fatal("kill -9 で、DB が消えている (残りの再現になっていない)")
	}
	second, id2 := start()
	if id1 == id2 {
		t.Fatal("別のセッションのはず")
	}
	waitUntil(t, "前の起動の残りの掃除", func() bool { return len(dbFilesIn(t, dir1)) == 0 })
	if len(dbFilesIn(t, sessionDirOf(f.stateDir(), id2))) == 0 {
		t.Fatal("生きている起動の DB を消した")
	}
	_ = second
}

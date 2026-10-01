package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// E2E (bwrap・非 root が要る): 実際の web のハンドラ (ログイン済みの cookie・関門つき) → web が起こす、実プロセスの goronation serve --chat → 実際の bwrap の檻 → 偽の claude (stream-json を話し、
// 許可された tool を、実際に実行する)。クレジットは使わない。Web からの開始 (POST /api/chat/start) → 指示 → 権限要求 → 世代の検査 → 再読み込みでの未決の復元 → 許可 (tool が実際に実行される) →
// 完了 → 次のターンで拒否 (実行されず、拒否が claude に伝わる) → 手動の終了。
//
// 偽の claude であって、実物の claude ではない (実物の claude は、認証・fake provider の経路が無いと、ターンを回せない。PR⑧ の PR 本文に、実物の確認の結果と範囲を書く)。

// killServes は、stateDir を使う、goronation serve (web が起こした子プロセス) を止める。
func killServes(t *testing.T, stateDir string) {
	t.Helper()
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte("serve\x00--state-dir\x00"+stateDir+"\x00")) {
			syscall.Kill(pid, syscall.SIGTERM)
		}
	}
}

// findFile は、dir の下で、名前 name のファイルを探す (最初の 1 つ)。
func findFile(dir, name string) string {
	var found string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && found == "" {
			found = p
		}
		return nil
	})
	return found
}

func TestE2EWebStartSendApproveDenyStop(t *testing.T) {
	f := newRunFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root では、chat を動かせない (errChatRoot)。CI の runner は非 root")
	}
	withTimeout(t, 240*time.Second)
	t.Setenv("HOME", f.home)
	t.Setenv("GORONATION_CLAUDE", f.exe)
	cw := newChatWebIn(t, defaultChatRelay, webReadTimeout, webWriteTimeout, f.dir, f.exe, f.stateDir())
	t.Cleanup(func() { killServes(t, f.stateDir()) })

	// 1. Web から開始する (最初の指示は、チャット欄から送る: 起動しただけでは、何も送らない)。
	code, body := cw.req("POST", "/api/chat/start", `{"repo":"repo"}`, nil)
	if code != 200 {
		t.Fatalf("start = %d %s", code, body)
	}
	var started struct{ ID string }
	if err := json.Unmarshal([]byte(body), &started); err != nil || !termIDRE.MatchString(started.ID) {
		t.Fatalf("応答 = %s", body)
	}
	id := started.ID
	if !chatSockExists(f.stateDir(), id) {
		t.Fatal("chat.sock が無い")
	}
	send := func(op, b string) (int, string) { return cw.req("POST", evPath(id, op), b, nil) }

	// セッション一覧に、chat.sock の有無 (会話を見られる) が出る。
	code, body = cw.req("GET", "/api/sessions", "", nil)
	var list []struct {
		ID   string `json:"id"`
		Chat bool   `json:"chat"`
	}
	if err := json.Unmarshal([]byte(body), &list); code != 200 || err != nil {
		t.Fatalf("sessions = %d %s", code, body)
	}
	foundSession := false
	for _, s := range list {
		if s.ID == id {
			foundSession = true
			if !s.Chat {
				t.Error("一覧の chat が false (chat.sock があるのに)")
			}
		}
	}
	if !foundSession {
		t.Errorf("一覧に %s が無い: %s", id, body)
	}

	// 2. SSE (hello の世代)。
	sse := cw.mustEvents(id)
	hello, _ := sse.next()
	var h struct {
		Generation string `json:"generation"`
		FirstSeq   uint64 `json:"first_seq"`
	}
	if hello.event != "hello" || json.Unmarshal([]byte(hello.data), &h) != nil || len(h.Generation) != 32 {
		t.Fatalf("hello = %+v", hello)
	}
	if strings.Contains(strings.Join(sse.raw, "\n"), `"turn.started"`) {
		t.Fatal("指示を送る前に、ターンが始まっていた")
	}

	// 3. 指示 → 権限要求 (Write)。
	if code, b := send("message", `{"text":"新しいファイルを作って"}`); code != 200 {
		t.Fatalf("message = %d %s", code, b)
	}
	sse.until(evType("turn.started"))
	if ss := sse.until(evType("session.started")); !strings.Contains(ss.data, `"permission_mode":"default"`) {
		t.Errorf("権限モードが default でない (承認なしで tool が動く): %s", ss.data)
	}
	req1 := sse.until(evType("permission.requested"))
	rid1 := requestIDFrom(t, req1.data)
	if rid1 != "perm-1" {
		t.Fatalf("request_id = %q", rid1)
	}

	// 4. 再読み込み (別の SSE): 未決の権限要求が、復元される (同じ世代・同じ request_id)。
	sse2 := cw.mustEvents(id)
	hello2, _ := sse2.next()
	var h2 struct{ Generation string }
	json.Unmarshal([]byte(hello2.data), &h2)
	if h2.Generation != h.Generation {
		t.Errorf("再読み込みで世代が変わった: %q → %q", h.Generation, h2.Generation)
	}
	if got := requestIDFrom(t, sse2.until(evType("permission.requested")).data); got != rid1 {
		t.Errorf("再読み込みで、未決の要求が復元されない: %q", got)
	}

	// 5. 世代の検査: 別の起動の世代・世代なしは、通らない。未決は動かない。
	hash1 := permHashFrom(t, req1.data)
	perm := func(gen, rid, hash, outcome string) (int, string) {
		b, _ := json.Marshal(map[string]string{"generation": gen, "request_id": rid, "outcome": outcome, "content_hash": hash})
		return send("permission", string(b))
	}
	if code, _ := perm("00000000000000000000000000000000", rid1, hash1, "allow_once"); code != 409 {
		t.Errorf("別の起動の世代 = %d (409 のはず)", code)
	}
	if code, _ := send("permission", `{"request_id":"`+rid1+`","outcome":"allow_once","content_hash":"`+hash1+`"}`); code != 400 {
		t.Errorf("世代なし = %d (400 のはず)", code)
	}
	if code, _ := send("message", `{"text":"割り込み"}`); code != 409 { // ターン中は、次の指示を受けない
		t.Errorf("ターン中の指示 = %d (409 のはず)", code)
	}
	if p := findFile(f.stateDir(), "new.txt"); p != "" {
		t.Fatalf("承認の前に、tool が実行された: %s", p)
	}

	// 6. 許可 → tool が実際に実行される (clone に new.txt ができる) → 完了。
	if code, b := perm(h2.Generation, rid1, hash1, "allow_once"); code != 200 {
		t.Fatalf("許可 = %d %s", code, b)
	}
	resolved := sse.until(evType("permission.resolved"))
	if !strings.Contains(resolved.data, `"allow_once"`) || !strings.Contains(resolved.data, `"human"`) {
		t.Errorf("決着 = %s", resolved.data)
	}
	upd := sse.until(evType("tool.update"))
	if !strings.Contains(upd.data, `"completed"`) {
		t.Errorf("tool.update = %s", upd.data)
	}
	txt := sse.until(func(f sseFrame) bool { return evType("message.text")(f) && strings.Contains(f.data, "request=perm-1") })
	if !strings.Contains(txt.data, `executed=true`) || !strings.Contains(txt.data, `\"behavior\":\"allow\"`) {
		t.Errorf("claude に、許可が伝わっていない: %s", txt.data)
	}
	sse.until(evType("turn.completed"))
	newTxt := findFile(f.stateDir(), "new.txt")
	if newTxt == "" {
		t.Fatal("許可したのに、tool が実行されていない (new.txt が無い)")
	}
	if b, _ := os.ReadFile(newTxt); string(b) != "turn 1\n" {
		t.Errorf("new.txt = %q", b)
	}
	if code, _ := perm(h.Generation, rid1, hash1, "allow_once"); code != 409 { // 二重の応答
		t.Errorf("二重の許可 = %d (409 のはず)", code)
	}

	// 7. 次のターン: 拒否 → 実行されず、拒否が claude に伝わる。
	if code, b := send("message", `{"text":"もう一度"}`); code != 200 {
		t.Fatalf("2 回目の指示 = %d %s", code, b)
	}
	req2 := sse.until(evType("permission.requested"))
	rid2, hash2 := requestIDFrom(t, req2.data), permHashFrom(t, req2.data)
	if rid2 != "perm-2" {
		t.Fatalf("request_id = %q", rid2)
	}
	if code, b := perm(h.Generation, rid2, hash2, "reject_once"); code != 200 {
		t.Fatalf("拒否 = %d %s", code, b)
	}
	deny := sse.until(func(f sseFrame) bool { return evType("message.text")(f) && strings.Contains(f.data, "request=perm-2") })
	if !strings.Contains(deny.data, `executed=false`) || !strings.Contains(deny.data, `\"behavior\":\"deny\"`) {
		t.Errorf("拒否が、claude に伝わっていない: %s", deny.data)
	}
	sse.until(evType("turn.completed"))
	if b, _ := os.ReadFile(newTxt); string(b) != "turn 1\n" {
		t.Errorf("拒否したのに、tool が実行された: new.txt = %q", b)
	}

	// 8. 手動の終了 → end。終了後は、指示も承認も受けない。
	if code, _ := send("stop", `{}`); code != 200 {
		t.Errorf("stop = %d", code)
	}
	end := sse.until(func(f sseFrame) bool { return f.event == "end" })
	if !strings.HasPrefix(end.data, `{"exit":`) {
		t.Errorf("end = %s", end.data)
	}
	if code, _ := send("message", `{"text":"終了後"}`); code != 409 {
		t.Errorf("終了後の指示 = %d (409 のはず)", code)
	}
}

// requestIDFrom は、permission.requested の Event (JSON) の data.request_id。
func requestIDFrom(t *testing.T, data string) string {
	t.Helper()
	var v struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatal(err)
	}
	return v.Data.RequestID
}

// permHashFrom は、permission.requested の data.content_hash (応答に写す。ADR 0042 決定 3: 必須)。
func permHashFrom(t *testing.T, data string) string {
	t.Helper()
	var v struct {
		Data struct {
			ContentHash string `json:"content_hash"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil || !strings.HasPrefix(v.Data.ContentHash, "sha256:") {
		t.Fatalf("content_hash が無い: %s (%v)", data, err)
	}
	return v.Data.ContentHash
}

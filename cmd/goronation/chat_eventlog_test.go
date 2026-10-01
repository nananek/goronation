//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
	"github.com/nananek/goronation/cmd/internal/eventlog"
)

const testLogSession = "20260101-000000-000000"

func sessionDirOf(state, id string) string {
	return filepath.Dir(termSocketPath(state, defaultGroup, id))
}

func dbFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "events.db") {
			out = append(out, e.Name())
		}
	}
	return out
}

// 変換は、型の写しだけ: Apply・Range・Trim が、Store の同じ操作に届く。
func TestChatEventStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := eventlog.Open(dir, "0123456789abcdef0123456789abcdef", eventlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(true)
	cs := chatEventStore{st}
	var evs []chat.StoredEvent
	for i := uint64(0); i < 5; i++ {
		evs = append(evs, chat.StoredEvent{Seq: i, Payload: []byte(fmt.Sprintf(`{"seq":%d,"type":"x","durable":true}`, i))})
	}
	if err := cs.Apply(chat.StoreOp{Append: evs, Pin: []uint64{1}}); err != nil {
		t.Fatal(err)
	}
	got, err := cs.Range(0, 4, 10)
	if err != nil || len(got) != 3 || got[0].Seq != 1 || got[2].Seq != 3 || string(got[1].Payload) != string(evs[2].Payload) {
		t.Fatalf("Range = %+v %v", got, err)
	}
	if _, err := cs.Trim(1); err != nil {
		t.Fatal(err)
	}
	got, _ = cs.Range(0, 5, 10)
	if len(got) == 0 || got[0].Seq != 1 { // 固定の行 (seq 1) は、Trim で消えない
		t.Fatalf("Trim のあと: %+v", got)
	}
}

// --no-event-log: nil・ディスクに何も作らない。nil の chatLog でも、メソッドは使える。
func TestOpenChatLogDisabledWritesNothing(t *testing.T) {
	state := t.TempDir()
	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, true, &buf)
	if lg != nil || err != nil {
		t.Fatalf("lg=%v err=%v", lg, err)
	}
	if ents, _ := os.ReadDir(state); len(ents) != 0 {
		t.Fatalf("何かを作った: %v", ents)
	}
	if s, d := lg.config(); s != nil || d != nil {
		t.Fatal("nil の config は、Store も OnDegrade も nil")
	}
	if err := lg.Close(chat.NewHub(chat.HubConfig{})); err != nil {
		t.Fatal(err)
	}
	lg.abort()
}

// DB は、セッションのディレクトリ (--socket と無関係: openChatLog は socket を受け取らない) に、0600 で作られ、Close で消える。
func TestOpenChatLogCreatesInSessionDirAndCloseRemoves(t *testing.T) {
	state := t.TempDir()
	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, false, &buf)
	if err != nil || lg == nil {
		t.Fatalf("lg=%v err=%v buf=%q", lg, err, buf.String())
	}
	dir := sessionDirOf(state, testLogSession)
	fi, err := os.Stat(filepath.Join(dir, "events.db"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("events.db: %v %v", fi, err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("ディレクトリの権限 = %v", di.Mode().Perm())
	}
	hub := chat.NewHub(chat.HubConfig{})
	hub.Close(0)
	if err := lg.Close(hub); err != nil {
		t.Fatal(err)
	}
	if left := dbFilesIn(t, dir); len(left) != 0 {
		t.Fatalf("Close のあとに残った: %v", left)
	}
	if buf.String() != "" {
		t.Fatalf("標準エラー出力に何か出た: %q", buf.String())
	}
}

// 別の serve が同じセッションの DB を持っていれば、起動の失敗 (errChatLogLocked)。
func TestOpenChatLogLockedIsStartFailure(t *testing.T) {
	state := t.TempDir()
	var buf syncBuffer
	first, err := openChatLog(state, testLogSession, false, &buf)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	defer first.abort()
	second, err := openChatLog(state, testLogSession, false, &buf)
	if !errors.Is(err, errChatLogLocked) || second != nil {
		t.Fatalf("second=%v err=%v", second, err)
	}
	if _, err := os.Stat(filepath.Join(sessionDirOf(state, testLogSession), "events.db")); err != nil {
		t.Fatal("持ち主の DB を消してはいけない:", err)
	}
}

// Open の失敗 (ErrLocked 以外) は、縮退して続ける: nil・error なし・標準エラー出力に 1 行。
func TestOpenChatLogOpenFailureDegrades(t *testing.T) {
	state := t.TempDir()
	dir := sessionDirOf(state, testLogSession)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("ファイル"), 0o600); err != nil { // セッションのディレクトリの場所が、ファイル
		t.Fatal(err)
	}
	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, false, &buf)
	if lg != nil || err != nil {
		t.Fatalf("lg=%v err=%v", lg, err)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "耐久ログを使わない") {
		t.Fatalf("標準エラー出力 = %q", out)
	}
}

// 起動ごとの掃除: 異常終了の残り (ロックを持つ者がいない DB) は消え、生きている持ち主 (ロックを持つ) の DB は消えない。
func TestOpenChatLogSweepsStaleButNotLive(t *testing.T) {
	state := t.TempDir()
	stale := sessionDirOf(state, "20260101-000000-000001")
	live := sessionDirOf(state, "20260101-000000-000002")
	for _, d := range []string{stale, live} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := eventlog.Open(stale, "aaaaaaaaaaaaaaaa", eventlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close(false) // kill -9 の残りの代わり: ファイルを残して、ロックを手放す
	if len(dbFilesIn(t, stale)) == 0 {
		t.Fatal("残りが作れていない")
	}
	liveStore, err := eventlog.Open(live, "bbbbbbbbbbbbbbbb", eventlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer liveStore.Close(true)

	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, false, &buf)
	if err != nil || lg == nil {
		t.Fatalf("lg=%v err=%v buf=%q", lg, err, buf.String())
	}
	defer lg.abort()
	if left := dbFilesIn(t, stale); len(left) != 0 {
		t.Fatalf("異常終了の残りが掃除されていない: %v", left)
	}
	if len(dbFilesIn(t, live)) == 0 {
		t.Fatal("生きている DB を消した")
	}
}

// 全セッションの合計が上限以上なら、この起動は書かない (縮退・標準エラー出力・DB を作らない)。
func TestOpenChatLogOverTotalLimitDegrades(t *testing.T) {
	state := t.TempDir()
	big := sessionDirOf(state, "20260101-000000-000003")
	if err := os.MkdirAll(big, 0o700); err != nil {
		t.Fatal(err)
	}
	dirf, err := os.Open(big) // 生きている持ち主の代わり: ディレクトリの flock を持つ
	if err != nil {
		t.Fatal(err)
	}
	defer dirf.Close()
	if err := syscall.Flock(int(dirf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(big, "events.db"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(chatLogTotalLimit); err != nil { // スパースファイル
		t.Fatal(err)
	}
	f.Close()

	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, false, &buf)
	if lg != nil || err != nil {
		t.Fatalf("lg=%v err=%v", lg, err)
	}
	if !strings.Contains(buf.String(), "上限") {
		t.Fatalf("標準エラー出力 = %q", buf.String())
	}
	if len(dbFilesIn(t, sessionDirOf(state, testLogSession))) != 0 {
		t.Fatal("縮退したのに、DB を作った")
	}
}

// OnDegrade の reason は、Store の error (path を含みうる) を含む: 運用者の端末への 1 行にし、制御列・行の偽造 (改行) を通さない。
func TestChatLogOnDegradeIsSanitizedOneLine(t *testing.T) {
	state := t.TempDir()
	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, false, &buf)
	if err != nil || lg == nil {
		t.Fatal(err)
	}
	defer lg.abort()
	_, onDegrade := lg.config()
	onDegrade("書き込みの失敗: open /x/y: \x1b[2Jdenied\ngoronation serve: /tmp/a.sock で待ち受けている (session=evil)\x07")
	out := buf.String()
	if strings.Count(out, "\n") != 1 || strings.ContainsAny(out, "\x1b\x07") || strings.Contains(out, "\ngoronation serve: /tmp") {
		t.Fatalf("無害化されていない: %q", out)
	}
	if !strings.HasPrefix(out, "goronation serve: 耐久ログを止めた") {
		t.Fatalf("out = %q", out)
	}
}

// 終了: Hub が閉じたあと、書き手を待ってから、DB・-wal・-shm を消す。キューに残った分は、消す前に書き終える (待つ)。
func TestChatLogCloseWaitsForWriterThenRemoves(t *testing.T) {
	state := t.TempDir()
	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, false, &buf)
	if err != nil {
		t.Fatal(err)
	}
	store, onDegrade := lg.config()
	hub := chat.NewHub(chat.HubConfig{Store: store, OnDegrade: onDegrade})
	for i := uint64(0); i < 50; i++ {
		hub.Publish(durableEvent(i, 100))
	}
	hub.Close(0)
	if err := lg.Close(hub); err != nil {
		t.Fatal(err)
	}
	if left := dbFilesIn(t, sessionDirOf(state, testLogSession)); len(left) != 0 {
		t.Fatalf("残った: %v", left)
	}
	if strings.Contains(buf.String(), "止めた") {
		t.Fatalf("閉じる途中で縮退した: %q", buf.String())
	}
}

// durableEvent は、Store に入れてよい形の Event (seq・type・durable を持つ JSON 1 行)。pad は、大きさ (バイト) の目安。
func durableEvent(seq uint64, pad int) chat.Event {
	j := fmt.Sprintf(`{"seq":%d,"type":"assistant.text","durable":true,"data":{"pad":"%s"}}`, seq, strings.Repeat("x", pad))
	return chat.Event{Seq: seq, Type: "assistant.text", Durable: true, JSON: []byte(j)}
}

// hubChat は、耐久ログつきの Hub だけを持つ chatSession (エージェントは無い。Event は、テストが Hub に直接足す)。
func hubChat(t *testing.T, state string, hub chat.HubConfig, noLog bool, limit int) (*chatSession, *chatLog, *syncBuffer, *httptest.Server) {
	t.Helper()
	var buf syncBuffer
	lg, err := openChatLog(state, testLogSession, noLog, &buf)
	if err != nil {
		t.Fatal(err)
	}
	store, onDegrade := lg.config()
	s := &chatSession{id: testLogSession, log: lg, done: make(chan struct{})}
	s.Chat = chat.NewSession(chat.SessionConfig{Launch: mustLaunch(t), ID: testLogSession, Input: io.Discard, Hub: hub, Store: store, OnDegrade: onDegrade})
	srv := httptest.NewServer(newChatHandlerWith(s, limit))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		s.Chat.Conv.Stop()
		s.Chat.Hub.Close(0)
		lg.Close(s.Chat.Hub)
	})
	return s, lg, &buf, srv
}

// publishWritten は、from..to-1 の Event を、書き手が追いつくのを待ちながら足す (リングの押し出しに、書き込みが間に合わず縮退しない)。
func publishWritten(t *testing.T, s *chatSession, lg *chatLog, from, to uint64, pad int) {
	t.Helper()
	for i := from; i < to; i++ {
		s.Chat.Hub.Publish(durableEvent(i, pad))
		if (i-from)%4 == 3 || i == to-1 {
			waitUntil(t, "書き込み", func() bool {
				rows, err := lg.store.Range(i-1, i+1, 2)
				return err == nil && len(rows) == 1 && rows[0].Seq == i
			})
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s を待つ間に時間切れ", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// events は、GET /events?<query> を開く (headers は足す)。
func getEvents(t *testing.T, srv *httptest.Server, query string, headers map[string]string) *sseReader {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/events"+query, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /events%s: %v %v", query, resp, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return newSSEReader(t, resp.Body)
}

func helloOf(t *testing.T, r *sseReader) map[string]any {
	t.Helper()
	f, ok := r.next()
	if !ok || f.event != "hello" || f.id != "" {
		t.Fatalf("hello = %+v %v (hello に id: は付けない)", f, ok)
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(f.data), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// readSeqs は、n 件の Event の seq を読む (id: と data の seq が一致する)。
func readSeqs(t *testing.T, r *sseReader, n int) []uint64 {
	t.Helper()
	var out []uint64
	for len(out) < n {
		f, ok := r.next()
		if !ok {
			t.Fatalf("%d 件目の前に、ストリームが終わった (読めた: %v)", len(out)+1, out)
		}
		if f.event != "" {
			t.Fatalf("イベント以外: %+v", f)
		}
		var v struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(f.data), &v); err != nil {
			t.Fatal(err)
		}
		if f.id != strconv.FormatUint(v.Seq, 10) {
			t.Fatalf("id: %q と data の seq %d が一致しない", f.id, v.Seq)
		}
		out = append(out, v.Seq)
	}
	return out
}

func contiguous(t *testing.T, seqs []uint64, first uint64) {
	t.Helper()
	for i, s := range seqs {
		if s != first+uint64(i) {
			t.Fatalf("seq が連続しない (位置 %d: %d。期待 %d)。全体: %v", i, s, first+uint64(i), seqs)
		}
	}
}

func newSSEReader(t *testing.T, body io.ReadCloser) *sseReader {
	sc := bufio.NewScanner(body)
	sc.Buffer(nil, 4<<20)
	return &sseReader{t: t, body: body, sc: sc}
}

const testGenerationFmt = "&generation=%s"

func (s *chatSession) generation() string { return s.Chat.Conv.Generation() }

func resumeQuery(after uint64, gen string) string {
	return fmt.Sprintf("?after=%d"+testGenerationFmt, after, gen)
}

// E2E (リングを溢れる出力 → 切断 → after で再接続 → 欠けなし・重複なし・省略の印なし): リングは数件分・DB に全部ある。
func TestEventsResumeAfterRingOverflowHasNoGapNoDuplicate(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, lg, _, srv := hubChat(t, t.TempDir(), chat.HubConfig{MaxBytes: 4 << 10}, false, chatBackfillMaxBytes)
	publishWritten(t, s, lg, 0, 200, 200)

	// 全再送は、リングの分だけ (先頭は 0 でない)。resumed=false。
	full := getEvents(t, srv, "", nil)
	h := helloOf(t, full)
	if h["resumed"] != false || h["durable"] != true || h["first_seq"].(float64) == 0 {
		t.Fatalf("全再送の hello = %v", h)
	}
	full.body.Close()

	// 画面が、seq 49 まで読んで切れた。after=49 で繋ぎ直す: 50 から、欠けず・重複せずに、199 まで。
	r := getEvents(t, srv, resumeQuery(49, s.generation()), nil)
	h = helloOf(t, r)
	if h["resumed"] != true || h["durable"] != true || h["first_seq"].(float64) != 0 || h["generation"] != s.generation() {
		t.Fatalf("hello = %v (削っていなければ first_seq は 0)", h)
	}
	contiguous(t, readSeqs(t, r, 150), 50)

	// 続けて足した分は、ライブで、続きとして届く (境目に欠けも重複も無い)。
	publishWritten(t, s, lg, 200, 210, 200)
	contiguous(t, readSeqs(t, r, 10), 200)
}

// エージェントが終わっても (Hub が閉じても)、serve が生きている間は、after で再接続できる: 続きと end が届く。
func TestEventsResumeAfterHubClosed(t *testing.T) {
	withTimeout(t, 30*time.Second)
	s, lg, _, srv := hubChat(t, t.TempDir(), chat.HubConfig{MaxBytes: 4 << 10}, false, chatBackfillMaxBytes)
	publishWritten(t, s, lg, 0, 60, 200)
	s.Chat.Hub.Close(7)
	r := getEvents(t, srv, resumeQuery(9, s.generation()), nil)
	if h := helloOf(t, r); h["resumed"] != true {
		t.Fatalf("hello = %v", h)
	}
	contiguous(t, readSeqs(t, r, 50), 10)
	f, ok := r.next()
	if !ok || f.event != "end" || f.id != "" || f.data != `{"exit":7}` {
		t.Fatalf("end = %+v %v", f, ok)
	}
}

// 世代の不一致・不正な after・after が発行済みの範囲外・キーの重複・Last-Event-ID だけ → エラーにならず、全再送 (resumed=false)。
func TestEventsFallsBackToFullReplay(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, lg, _, srv := hubChat(t, t.TempDir(), chat.HubConfig{MaxBytes: 4 << 10}, false, chatBackfillMaxBytes)
	publishWritten(t, s, lg, 0, 100, 200)
	g := s.generation()
	other := strings.Repeat("0", len(g))
	for name, c := range map[string]struct {
		query   string
		headers map[string]string
	}{
		"世代の不一致":       {resumeQuery(10, other), nil},
		"世代が無い":        {"?after=10", nil},
		"after が無い":    {"?generation=" + g, nil},
		"after が非数":    {"?after=abc&generation=" + g, nil},
		"after が負":     {"?after=-1&generation=" + g, nil},
		"after が小数":    {"?after=1.5&generation=" + g, nil},
		"after が指数":    {"?after=1e3&generation=" + g, nil},
		"after が空":     {"?after=&generation=" + g, nil},
		"after が 17 桁": {"?after=10000000000000000&generation=" + g, nil},
		"after が 2^53": {"?after=9007199254740992&generation=" + g, nil},
		"after が未発行":   {resumeQuery(100, g), nil},
		"after が巨大":    {resumeQuery(9007199254740991, g), nil},
		"after が重複":    {"?after=10&after=20&generation=" + g, nil},
		"世代が重複":        {"?after=10&generation=" + g + "&generation=" + g, nil},
		"ヘッダだけ":        {"", map[string]string{"Last-Event-ID": "10"}},
		"ヘッダ + 世代":     {"?generation=" + g, map[string]string{"Last-Event-ID": "10"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := getEvents(t, srv, c.query, c.headers)
			h := helloOf(t, r)
			if h["resumed"] != false {
				t.Fatalf("全再送になっていない: %v", h)
			}
			if first, _ := r.next(); first.id == "" || first.id == "10" || first.id == "11" {
				t.Fatalf("先頭 = %+v (リングの先頭から全部)", first)
			}
			r.body.Close()
		})
	}
	// 世代が正しい after=99 (最後の seq) は、効く。
	if h := helloOf(t, getEvents(t, srv, resumeQuery(99, g), nil)); h["resumed"] != true {
		t.Fatalf("hello = %v", h)
	}
}

// GC: 起動あたりの上限を超えて古い行が削られると、first_seq > after+1 になり、画面が省略を検出できる。届くのは、残った行から (欠けは、省略として見える)。
func TestEventsGCOmissionIsVisibleInFirstSeq(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, lg, _, srv := hubChat(t, t.TempDir(), chat.HubConfig{MaxBytes: 4 << 10, StoreMaxBytes: 16 << 10}, false, chatBackfillMaxBytes)
	publishWritten(t, s, lg, 0, 300, 200)
	waitUntil(t, "GC (古い行が削られる)", func() bool {
		rows, err := lg.store.Range(0, 3, 4)
		return err == nil && len(rows) == 0
	})
	r := getEvents(t, srv, resumeQuery(0, s.generation()), nil)
	h := helloOf(t, r)
	first := uint64(h["first_seq"].(float64))
	if h["resumed"] != true || first <= 1 {
		t.Fatalf("hello = %v (削ったので first_seq > after+1)", h)
	}
	seqs := readSeqs(t, r, 5)
	if seqs[0] < first {
		t.Fatalf("first_seq=%d より前の seq が届いた: %v", first, seqs)
	}
}

// 1 接続の Backfill の上限: 超えたら閉じる (何も足さない)。画面は、進んだ after で繋ぎ直す: 1 回ごとに前進し、いつかは追いつく (ライブロックしない)。
func TestEventsBackfillLimitClosesAndReconnectProgresses(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, lg, _, srv := hubChat(t, t.TempDir(), chat.HubConfig{MaxBytes: 2 << 10}, false, 8<<10)
	publishWritten(t, s, lg, 0, 200, 300)
	after, rounds := uint64(0), 0
	got := []uint64{}
	for len(got) < 150 {
		rounds++
		if rounds > 100 {
			t.Fatal("前進しない (ライブロック)")
		}
		r := getEvents(t, srv, resumeQuery(after, s.generation()), nil)
		helloOf(t, r)
		n := 0
		for {
			f, ok := r.next()
			if !ok {
				break
			}
			var v struct {
				Seq uint64 `json:"seq"`
			}
			json.Unmarshal([]byte(f.data), &v)
			if len(got) > 0 && v.Seq != got[len(got)-1]+1 {
				t.Fatalf("欠け・重複: %d の次に %d", got[len(got)-1], v.Seq)
			}
			got = append(got, v.Seq)
			after = v.Seq
			n++
			if v.Seq >= 199 || len(got) >= 150 {
				break
			}
		}
		r.body.Close()
		if n == 0 {
			t.Fatal("1 件も進まなかった")
		}
	}
	if rounds < 2 {
		t.Fatal("上限で閉じていない (1 回で全部届いた)")
	}
}

// Backfill が Store を読めない (閉じた) → 何も足さずに閉じる (error の文字列を出さない)・Hub は縮退する (次の接続は durable=false で全再送)。
func TestEventsBackfillStoreFailureClosesQuietlyAndDegrades(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, lg, buf, srv := hubChat(t, t.TempDir(), chat.HubConfig{MaxBytes: 2 << 10}, false, chatBackfillMaxBytes)
	publishWritten(t, s, lg, 0, 100, 200)
	lg.store.Close(false) // Range は ErrClosed
	r := getEvents(t, srv, resumeQuery(0, s.generation()), nil)
	helloOf(t, r)
	if f, ok := r.next(); ok {
		t.Fatalf("何も足さずに閉じるはず: %+v", f)
	}
	waitUntil(t, "縮退", func() bool { return !s.Chat.Hub.Durable() })
	waitUntil(t, "標準エラー出力", func() bool { return strings.Contains(buf.String(), "耐久ログを止めた") })
	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("縮退の通知は 1 回・1 行: %q", out)
	}
	h := helloOf(t, getEvents(t, srv, resumeQuery(0, s.generation()), nil))
	if h["durable"] != false || h["resumed"] != false {
		t.Fatalf("縮退のあとの hello = %v", h)
	}
}

// 書き込みの失敗 (Store が閉じた): 縮退して、標準エラー出力に出る。接続は止まらず、ライブは届き続ける (durable=false)。
func TestEventsDegradeOnWriteFailureKeepsStreaming(t *testing.T) {
	withTimeout(t, 60*time.Second)
	s, lg, buf, srv := hubChat(t, t.TempDir(), chat.HubConfig{}, false, chatBackfillMaxBytes)
	r := getEvents(t, srv, "", nil)
	if h := helloOf(t, r); h["durable"] != true {
		t.Fatalf("hello = %v", h)
	}
	lg.store.Close(false)
	s.Chat.Hub.Publish(durableEvent(0, 10))
	waitUntil(t, "縮退", func() bool { return !s.Chat.Hub.Durable() })
	waitUntil(t, "標準エラー出力", func() bool { return strings.Contains(buf.String(), "耐久ログを止めた") })
	contiguous(t, readSeqs(t, r, 1), 0)
	s.Chat.Hub.Publish(durableEvent(1, 10))
	contiguous(t, readSeqs(t, r, 1), 1)
	if h := helloOf(t, getEvents(t, srv, "", nil)); h["durable"] != false {
		t.Fatalf("縮退のあとの hello = %v", h)
	}
}

// --no-event-log: ディスクに何も書かれず、hello は durable=false・resumed=false (after が正しくても、全再送)。
func TestEventsWithoutEventLog(t *testing.T) {
	withTimeout(t, 30*time.Second)
	state := t.TempDir()
	s, _, _, srv := hubChat(t, state, chat.HubConfig{MaxBytes: 4 << 10}, true, chatBackfillMaxBytes)
	for i := uint64(0); i < 50; i++ {
		s.Chat.Hub.Publish(durableEvent(i, 200))
	}
	h := helloOf(t, getEvents(t, srv, resumeQuery(10, s.generation()), nil))
	if h["durable"] != false || h["resumed"] != false {
		t.Fatalf("hello = %v", h)
	}
	if files := dbFilesIn(t, sessionDirOf(state, testLogSession)); len(files) != 0 {
		t.Fatalf("ディスクに書いた: %v", files)
	}
}

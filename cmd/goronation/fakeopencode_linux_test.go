//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 偽の opencode (serve --stdio --port N): /work/scene.ndjson (spec/testdata/golden/opencode-serve/<場面>/events.ndjson) を、SSE (GET /api/event) で
// 再生する。最初の行 (server.connected) は、すぐに流し、POST /api/session のあとに、残りを流す。人間の操作が起こす事象 (user の
// inbox.enqueued・permission.replied・form.replied・form.cancelled) の前では、対応する要求 (POST) が来るまで止まる。
// 受けた要求は、標準エラー出力に "REQ <引用した「METHOD path body」>" で出す。Authorization が、init の付ける Basic opencode:<トークン> でなければ 401
// (AUTH-BAD を出す)。golden の ID の記号 (<ses:1>) は、本物の形 (ses_1) に戻す (アダプタが、形を検査するため)。
//
// 檻の中で動く (chat の E2E。実物の opencode の無い CI で、serve --chat の経路を通す)。

var fakeTokenRE = regexp.MustCompile(`<([a-z]+):(\d+)>`)

func fakeOpencodeServe(args []string) int {
	realOut := os.Stdout // init が読む、起動の証明の 1 行
	os.Stdout = os.Stderr
	port := ""
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			port = args[i+1]
		}
	}
	if _, err := strconv.Atoi(port); err != nil {
		fmt.Fprintln(os.Stderr, "fake opencode: --port が無い")
		return 2
	}
	scene, err := os.ReadFile("/work/scene.ndjson")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake opencode: 場面を読めない:", err)
		return 2
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(fakeTokenRE.ReplaceAll(scene, []byte("${1}_${2}"))))
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines = append(lines, sc.Text())
		}
	}
	sessionID := ""
	for _, l := range lines {
		var e struct {
			Type string `json:"type"`
			Data struct {
				SessionID string `json:"sessionID"`
				ParentID  string `json:"parentID"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.Type == "session.created" && e.Data.ParentID == "" {
			sessionID = e.Data.SessionID
			break
		}
	}
	modeB, _ := os.ReadFile("/work/fake.mode") // 敵対的な動き (serve_chat_opencode_attack_linux_test.go): badroot・noconnected・createfail・noroot・preroot
	mode := strings.TrimSpace(string(modeB))
	token := os.Getenv("OPENCODE_PASSWORD")
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+token))

	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen-error="+err.Error())
		return 1
	}
	go func() {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	created := make(chan struct{})
	human := make(chan struct{}, 64)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			fmt.Fprintln(os.Stderr, "AUTH-BAD")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/api/event" {
			serveFakeEvents(w, r, lines, sessionID, mode, created, human)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		fmt.Fprintln(os.Stderr, "REQ "+strconv.Quote(r.Method+" "+r.URL.Path+" "+string(body)))
		if r.Method == "POST" && r.URL.Path == "/api/session" {
			w.Header().Set("Content-Type", "application/json")
			if mode == "createfail" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			id := sessionID
			if mode == "badroot" { // 作成の応答の id が、SSE の root と違う (ほかの利用者の session が root になる状況)
				id = "ses_mine"
			}
			fmt.Fprintf(w, `{"data":{"id":%q}}`, id)
			select {
			case <-created:
			default:
				close(created)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/session/") {
			human <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})
	if mode == "forgeready" { // 子が、ホストへの状態の行を偽造する試み: 自分の標準出力 (init の pipe) と、init の標準出力 (/proc/1/fd/1) に書く
		fmt.Fprintf(realOut, "{\"ready\":true}\n")
		if f, err := os.OpenFile("/proc/1/fd/1", os.O_WRONLY, 0); err == nil {
			fmt.Fprintf(f, "{\"ready\":true}\n")
			fmt.Fprintln(os.Stderr, "FORGE-PROC1-OPENED")
			f.Close()
		} else {
			fmt.Fprintln(os.Stderr, "FORGE-PROC1-DENIED")
		}
		time.Sleep(3 * time.Second)
	}
	fmt.Fprintf(realOut, "{\"url\":\"http://127.0.0.1:%s\"}\n", port)
	srv := &http.Server{Handler: mux}
	srv.Serve(l)
	return 0
}

// fakeGate は、人間の操作の後にしか起きない SSE の事象か。
func fakeGate(line, root string) bool {
	var e struct {
		Type string `json:"type"`
		Data struct {
			SessionID string `json:"sessionID"`
			Item      struct {
				Type string `json:"type"`
			} `json:"item"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(line), &e) != nil {
		return false
	}
	switch e.Type {
	case "session.inbox.enqueued":
		return e.Data.Item.Type == "user" && e.Data.SessionID == root // 子の session への指示 (サブエージェントの prompt) は、opencode が自分で出す
	case "permission.replied", "form.replied", "form.cancelled":
		return true
	}
	return false
}

func serveFakeEvents(w http.ResponseWriter, r *http.Request, lines []string, root, mode string, created, human chan struct{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	send := func(l string) bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", l); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if mode == "noconnected" { // 最初のイベントが、server.connected でない
		send(lines[1])
		<-r.Context().Done()
		return
	}
	if len(lines) == 0 || !send(lines[0]) {
		return
	}
	late := false // SSE を、session の作成のあとに開いた (実物の opencode は、作成の事象を、その時に繋がっていた SSE にだけ流す)
	select {
	case <-created:
		late = true
	default:
		select {
		case <-created:
		case <-r.Context().Done():
			return
		}
	}
	if mode == "noroot" { // root の session.created が、来ない (ほかのイベントだけが流れる)
		for i := 0; ; i++ {
			if !send(fmt.Sprintf(`{"type":"project.updated","data":{"n":%d}}`, i)) {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	if mode == "preroot" { // root の前に、偽の承認の要求 (まだ root が分からない)
		send(fmt.Sprintf(`{"type":"permission.asked","data":{"id":"per_evil","sessionID":%q,"action":"shell","resources":["rm -rf /"]}}`, root))
	}
	skipping := late
	for _, l := range lines[1:] {
		if skipping { // 取りこぼした事象 (root の session.created まで)
			var e struct {
				Type string `json:"type"`
			}
			json.Unmarshal([]byte(l), &e)
			skipping = e.Type != "session.created"
			continue
		}
		if mode == "ssecut" && fakeGate(l, root) { // 最初の人間の操作の前で、SSE を切る (opencode が落ちる・切断する)
			fmt.Fprintln(os.Stderr, "SSE-CUT")
			return
		}
		if fakeGate(l, root) {
			select {
			case <-human:
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Second):
				fmt.Fprintln(os.Stderr, "GATE-TIMEOUT")
				return
			}
		}
		if !send(l) {
			fmt.Fprintln(os.Stderr, "SSE-CLOSED")
			return
		}
	}
	fmt.Fprintln(os.Stderr, "SCENE-DONE")
	<-r.Context().Done()
	fmt.Fprintln(os.Stderr, "SSE-CLOSED")
}

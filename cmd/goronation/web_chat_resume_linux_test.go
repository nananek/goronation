package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testGen = "0123456789abcdef0123456789abcdef"

// web は、after・generation を検証して、url.Values で作り直す: 正しい (どちらも 1 つ・形が合う) ときだけ、その 2 つだけを serve に渡す。
// 不正・欠け・重複・余分なキーは、serve に届かない (不正なら、クエリなし = 全再送)。
func TestChatEventsQueryRebuild(t *testing.T) {
	goodOut := "?after=42&generation=" + testGen
	for name, c := range map[string]struct{ in, want string }{
		"正しい":                {"after=42&generation=" + testGen, goodOut},
		"順序が逆":               {"generation=" + testGen + "&after=42", goodOut},
		"余分なキー":              {"after=42&generation=" + testGen + "&x=y&last_event_id=9", goodOut},
		"after が 0":          {"after=0&generation=" + testGen, "?after=0&generation=" + testGen},
		"after が 16 桁":       {"after=9999999999999999&generation=" + testGen, "?after=9999999999999999&generation=" + testGen},
		"after が 17 桁":       {"after=99999999999999999&generation=" + testGen, ""},
		"after が負":           {"after=-1&generation=" + testGen, ""},
		"after が非数":          {"after=abc&generation=" + testGen, ""},
		"after が空":           {"after=&generation=" + testGen, ""},
		"after が小数":          {"after=1.5&generation=" + testGen, ""},
		"after に空白":          {"after=%201&generation=" + testGen, ""},
		"after に改行":          {"after=1%0d%0aX-A:b&generation=" + testGen, ""},
		"after が全角数字":        {"after=%EF%BC%91&generation=" + testGen, ""},
		"after が重複":          {"after=1&after=2&generation=" + testGen, ""},
		"generation が重複":     {"after=1&generation=" + testGen + "&generation=" + testGen, ""},
		"generation が短い":     {"after=1&generation=0123", ""},
		"generation が長い":     {"after=1&generation=" + testGen + "0", ""},
		"generation が大文字":    {"after=1&generation=" + strings.ToUpper(testGen), ""},
		"generation が非 16 進": {"after=1&generation=" + strings.Repeat("g", 32), ""},
		"generation に制御文字":   {"after=1&generation=" + testGen[:30] + "%0a%0a", ""},
		"after だけ":           {"after=1", ""},
		"generation だけ":      {"generation=" + testGen, ""},
		"空":                  {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := url.ParseQuery(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := chatEventsQuery(q); got != c.want {
				t.Fatalf("chatEventsQuery(%q) = %q (%q のはず)", c.in, got, c.want)
			}
		})
	}
}

// web 経由: serve に届く要求のクエリは、検証済みの after・generation だけ。Last-Event-ID のヘッダは、転送しない。
func TestWebChatForwardsOnlyValidatedQuery(t *testing.T) {
	withTimeout(t, 60*time.Second)
	cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
	var mu sync.Mutex
	var seen []string
	var lastEventID []string
	fakeUpstream(t, cw, chatTestID, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RawQuery)
		lastEventID = append(lastEventID, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: hello\ndata: {}\n\n")
		w.(http.Flusher).Flush()
	})
	for _, c := range []struct{ query, want string }{
		{"after=7&generation=" + testGen + "&evil=1", "after=7&generation=" + testGen},
		{"after=7&after=8&generation=" + testGen, ""},
		{"after=x&generation=" + testGen, ""},
		{"", ""},
	} {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", cw.web.URL+"/s/"+chatTestID+"/events?"+c.query, nil)
		req.Header.Set("Last-Event-ID", "5")
		resp, err := cw.client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%q: %v %v", c.query, resp, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		mu.Lock()
		got, hdr := seen[len(seen)-1], lastEventID[len(lastEventID)-1]
		mu.Unlock()
		if got != c.want {
			t.Errorf("クエリ %q: serve に届いた RawQuery = %q (%q のはず)", c.query, got, c.want)
		}
		if hdr != "" {
			t.Errorf("Last-Event-ID が serve に届いた: %q", hdr)
		}
	}
}

// readSSEEvents: `id: <10 進数 16 桁まで>` は、1 イベントに 1 行だけ許す。それ以外の id 行 (数字以外・長すぎる・空白・複数・id:5) は、そこで止める。
func TestWebChatSSEIDLineAllowList(t *testing.T) {
	withTimeout(t, 60*time.Second)
	for name, c := range map[string]struct {
		stream string
		ok     bool
	}{
		"正しい":      {"id: 5\ndata: {}\n\n", true},
		"16 桁":     {"id: 9999999999999999\ndata: {}\n\n", true},
		"17 桁":     {"id: 99999999999999999\ndata: {}\n\n", false},
		"数字以外":     {"id: 5a\ndata: {}\n\n", false},
		"空":        {"id: \ndata: {}\n\n", false},
		"負":        {"id: -1\ndata: {}\n\n", false},
		"末尾の空白":    {"id: 5 \ndata: {}\n\n", false},
		"スペースなし":   {"id:5\ndata: {}\n\n", false},
		"複数の id 行": {"id: 5\nid: 6\ndata: {}\n\n", false},
		"retry 行":  {"retry: 1\ndata: {}\n\n", false},
		"大文字の ID:": {"ID: 5\ndata: {}\n\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			cw := newChatWeb(t, fastRelay(), webReadTimeout, webWriteTimeout)
			fakeUpstream(t, cw, chatTestID, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, "event: hello\ndata: {}\n\n"+c.stream+"event: end\ndata: {}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			sse := cw.mustEvents(chatTestID)
			if f, ok := sse.next(); !ok || f.event != "hello" {
				t.Fatalf("hello = %+v %v", f, ok)
			}
			f, ok := sse.next()
			if c.ok {
				if !ok || f.data != "{}" || f.id == "" {
					t.Fatalf("id つきのイベントが届かない: %+v %v", f, ok)
				}
			} else if ok {
				t.Fatalf("止まらずに、届いた: %+v", f)
			}
		})
	}
}

package chat

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSSEReaderExtractsDataOnly(t *testing.T) {
	in := ": comment\nevent: x\nid: 1\nretry: 5\n\ndata: {\"a\":1}\n\ndata:{\"b\":2}\nfoo\n"
	sr := NewSSEReader(strings.NewReader(in))
	var got []string
	for {
		d, err := sr.Next()
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
		got = append(got, string(d))
	}
	if strings.Join(got, "|") != `{"a":1}|{"b":2}` {
		t.Fatalf("%q", got)
	}
}

func TestSSEReaderRejectsBadLines(t *testing.T) {
	sr := NewSSEReader(strings.NewReader("data: a\x00b\ndata: ok\n" + "data: " + strings.Repeat("x", DefaultMaxLine+10) + "\ndata: after\n"))
	var errs []error
	var got []string
	for {
		d, err := sr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		got = append(got, string(d))
	}
	if len(errs) != 2 || !errors.Is(errs[0], ErrBadLine) || !errors.Is(errs[1], ErrLineTooLong) || strings.Join(got, "|") != "ok|after" {
		t.Fatalf("errs=%v got=%q", errs, got)
	}
}

func sessionCreated(id, parent string) string {
	p := ""
	if parent != "" {
		p = fmt.Sprintf(`,"parentID":%q`, parent)
	}
	return fmt.Sprintf(`{"type":"session.created","data":{"sessionID":%q%s,"location":{"directory":"/work"},"version":"2.0.20"}}`, id, p)
}

func openCodeSession(t *testing.T) *Session {
	t.Helper()
	l, err := Agent("opencode")
	if err != nil {
		t.Fatal(err)
	}
	return NewSession(SessionConfig{Launch: l, ID: "s", Input: io.Discard})
}

func eventTypes(t *testing.T, s *Session) []string {
	t.Helper()
	sub, err := s.Hub.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	var out []string
	for _, e := range sub.Snapshot {
		out = append(out, e.Type)
	}
	return out
}

// root の session.created が、作成した session と一致したときだけ、会話に渡す。それより前の行は、渡さない。
func TestReadSSEPassesOnlyAfterRootMatches(t *testing.T) {
	s := openCodeSession(t)
	in := "data: " + `{"type":"permission.asked","data":{"id":"per_1","sessionID":"ses_a","action":"shell","resources":["ls"]}}` + "\n\n" +
		"data: " + sessionCreated("ses_a", "") + "\n\n" +
		"data: " + `{"type":"session.text.ended","data":{"sessionID":"ses_a","text":"hi"}}` + "\n\n"
	rooted := 0
	err := s.ReadSSE(NewSSEReader(strings.NewReader(in)), "ses_a", func() { rooted++ })
	if err != io.EOF || rooted != 1 {
		t.Fatalf("err=%v rooted=%d", err, rooted)
	}
	if got := strings.Join(eventTypes(t, s), ","); got != "session.started,message.text" {
		t.Fatalf("event = %s (root の前の permission.asked は渡さない)", got)
	}
}

// onRoot の時点で、root の session.created は会話へ渡し済み (onRoot の後に会話を使う側が、root 未確定に当たらない)。
func TestReadSSEOnRootAfterRootDelivered(t *testing.T) {
	s := openCodeSession(t)
	var at []string
	err := s.ReadSSE(NewSSEReader(strings.NewReader("data: "+sessionCreated("ses_a", "")+"\n\n")), "ses_a", func() { at = eventTypes(t, s) })
	if err != io.EOF || strings.Join(at, ",") != "session.started" {
		t.Fatalf("err=%v onRoot の時点の event = %v", err, at)
	}
}

// 最初の root が違うなら、何も渡さず、ErrRootMismatch で止まる (ほかの利用者の session を、会話にしない)。
func TestReadSSERootMismatchFailsClosed(t *testing.T) {
	s := openCodeSession(t)
	in := "data: " + sessionCreated("ses_other", "") + "\n\n" +
		"data: " + `{"type":"permission.asked","data":{"id":"per_1","sessionID":"ses_other","action":"shell","resources":["ls"]}}` + "\n\n"
	called := false
	err := s.ReadSSE(NewSSEReader(strings.NewReader(in)), "ses_mine", func() { called = true })
	if !errors.Is(err, ErrRootMismatch) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	if got := eventTypes(t, s); len(got) != 0 {
		t.Fatalf("渡った: %v", got)
	}
}

// 子の session.created (parentID あり) は、root と見なさない。
func TestReadSSEChildCreatedIsNotRoot(t *testing.T) {
	s := openCodeSession(t)
	in := "data: " + sessionCreated("ses_child", "ses_a") + "\n\n" + "data: " + sessionCreated("ses_a", "") + "\n\n"
	if err := s.ReadSSE(NewSSEReader(strings.NewReader(in)), "ses_a", nil); err != io.EOF {
		t.Fatal(err)
	}
	if got := strings.Join(eventTypes(t, s), ","); got != "session.started" {
		t.Fatalf("event = %s", got)
	}
}

// root が来ないまま、無限に行が来ても、呼び手が body を閉じれば終わる (ここでは、読み取りの error)。何も渡らない。
func TestReadSSENeverRootedPassesNothing(t *testing.T) {
	s := openCodeSession(t)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- s.ReadSSE(NewSSEReader(pr), "ses_a", nil) }()
	for i := 0; i < 50; i++ {
		fmt.Fprintf(pw, "data: {\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"ses_a\",\"text\":\"x\"}}\n\n")
	}
	pw.CloseWithError(errors.New("closed"))
	select {
	case err := <-done:
		if err == nil || err.Error() != "closed" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("終わらない")
	}
	if got := eventTypes(t, s); len(got) != 0 {
		t.Fatalf("渡った: %v", got)
	}
}

func FuzzSSEReader(f *testing.F) {
	f.Add([]byte("data: {\"type\":\"server.connected\"}\n\n"))
	f.Add([]byte(": c\nevent: x\ndata:\n\x00data: y\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		sr := NewSSEReader(strings.NewReader(string(in)))
		for i := 0; i < 1<<16; i++ {
			d, err := sr.Next()
			if err == io.EOF {
				return
			}
			if err == nil && (strings.ContainsRune(string(d), 0) || len(d) > DefaultMaxLine) {
				t.Fatalf("不正な本文: %q", d)
			}
		}
	})
}

// parentID が null・文字列でない・空でない session.created は、root にしない (アダプタの判定と合わせる)。
func TestParentlessSessionCreatedMatchesAdapter(t *testing.T) {
	for in, want := range map[string]bool{
		`{"type":"session.created","data":{"sessionID":"ses_a"}}`:                    true,
		`{"type":"session.created","data":{"sessionID":"ses_a","parentID":""}}`:      true,
		`{"type":"session.created","data":{"sessionID":"ses_a","parentID":null}}`:    false,
		`{"type":"session.created","data":{"sessionID":"ses_a","parentID":7}}`:       false,
		`{"type":"session.created","data":{"sessionID":"ses_a","parentID":"ses_b"}}`: false,
		`{"type":"session.updated","data":{"sessionID":"ses_a"}}`:                    false,
	} {
		if got, _ := parentlessSessionCreated([]byte(in)); got != want {
			t.Errorf("%s: root=%v want %v", in, got, want)
		}
	}
}

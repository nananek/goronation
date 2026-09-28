package termrelay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/termrelay/termrelaytest"
)

// newTestServer は、Accept するだけのハンドラを立てる httptest.Server と、そのハンドラが受け取った
// *Conn を通知する channel を返す。
func newTestServer(t *testing.T) (srv *httptest.Server, conns chan *Conn) {
	t.Helper()
	conns = make(chan *Conn, 1)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			t.Logf("Accept: %v", err)
			return
		}
		conns <- c
	}))
	t.Cleanup(srv.Close)
	return srv, conns
}

func TestReadLoopDispatchesBinaryAndResize(t *testing.T) {
	srv, conns := newTestServer(t)
	ctx := t.Context()
	cli, _, err := termrelaytest.Dial(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	c := <-conns

	var gotData [][]byte
	var gotResize [][2]int
	done := make(chan error, 1)
	go func() {
		done <- c.ReadLoop(ctx, func(data []byte) {
			gotData = append(gotData, data)
		}, func(cols, rows int) {
			gotResize = append(gotResize, [2]int{cols, rows})
		})
	}()

	if err := cli.WriteBinary(ctx, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := cli.WriteResize(ctx, 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := cli.WriteBinary(ctx, []byte("world")); err != nil {
		t.Fatal(err)
	}
	cli.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ReadLoop が終わらない")
	}
	if len(gotData) != 2 || string(gotData[0]) != "hello" || string(gotData[1]) != "world" {
		t.Fatalf("gotData = %q", gotData)
	}
	if len(gotResize) != 1 || gotResize[0] != [2]int{80, 24} {
		t.Fatalf("gotResize = %v", gotResize)
	}
}

func TestReadLoopRejectsBrokenResizeJSON(t *testing.T) {
	srv, conns := newTestServer(t)
	ctx := t.Context()
	cli, _, err := termrelaytest.Dial(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	c := <-conns

	done := make(chan error, 1)
	go func() { done <- c.ReadLoop(ctx, func([]byte) {}, func(int, int) {}) }()

	if err := cli.WriteText(ctx, "not json"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnsupportedMessage) {
			t.Fatalf("err = %v, want ErrUnsupportedMessage", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadLoop が終わらない")
	}
}

func TestReadLoopRejectsInvalidResizeValues(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cols, rows int
	}{
		{"zero", 0, 24}, {"negative", 80, -1}, {"too-large", 100001, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, conns := newTestServer(t)
			ctx := t.Context()
			cli, _, err := termrelaytest.Dial(ctx, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			c := <-conns

			done := make(chan error, 1)
			go func() { done <- c.ReadLoop(ctx, func([]byte) {}, func(int, int) {}) }()

			if err := cli.WriteResize(ctx, tc.cols, tc.rows); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrUnsupportedMessage) {
					t.Fatalf("err = %v, want ErrUnsupportedMessage", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ReadLoop が終わらない")
			}
		})
	}
}

func TestWriteBinaryRoundTrip(t *testing.T) {
	srv, conns := newTestServer(t)
	ctx := t.Context()
	cli, _, err := termrelaytest.Dial(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	c := <-conns

	if err := c.WriteBinary(ctx, []byte("pty output")); err != nil {
		t.Fatal(err)
	}
	got, err := cli.ReadBinary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pty output" {
		t.Fatalf("got = %q", got)
	}
}

// TestAcceptRejectsCrossOrigin は、Origin ヘッダが要求の Host と一致しない handshake を、ライブラリの
// 既定の同一オリジン確認が拒むことを確かめる (doc.go の「same-origin」を参照)。
func TestAcceptRejectsCrossOrigin(t *testing.T) {
	srv, conns := newTestServer(t)
	ctx := t.Context()
	header := http.Header{"Origin": []string{"https://evil.example"}}
	_, resp, err := termrelaytest.Dial(ctx, srv.URL, header)
	if err == nil {
		t.Fatal("クロスオリジンの handshake が通った")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	select {
	case <-conns:
		t.Fatal("Accept が通ってしまった")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCloseSendsCloseFrame(t *testing.T) {
	srv, conns := newTestServer(t)
	ctx := t.Context()
	cli, _, err := termrelaytest.Dial(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := <-conns

	// クライアント側が Read でブロックしていないと、受け取った close フレームへの自動応答が起きず、
	// サーバー側の Close (close handshake を待つ) がタイムアウトしてしまう。
	readErr := make(chan error, 1)
	go func() { _, err := cli.ReadBinary(ctx); readErr <- err }()

	if err := c.Close(StatusNormalClosure, "テスト"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("Close 後も読めた")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("クライアント側が Close を検知しない")
	}
}

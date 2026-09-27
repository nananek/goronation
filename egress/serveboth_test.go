package egress

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// dialLine は、l に接続し、line (末尾に "\r\n" を付けて) を送る。応答の最初の 1 行 (状態行) を返す。
func dialLine(t testing.TB, addr, line string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(line + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func newTestListener(t testing.TB) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// TestServeBothDispatchesConnect は、CONNECT が、既存の Server (connect) の処理 (許可リスト・監査) に、そのまま届くことを確かめる。
func TestServeBothDispatchesConnect(t *testing.T) {
	var auditBuf strings.Builder
	var mu sync.Mutex
	srv := New(Config{Allow: []string{"example.invalid:443"}, Audit: writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		auditBuf.Write(p)
		return len(p), nil
	})})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("git ハンドラが呼ばれた: %s %s", r.Method, r.RequestURI)
	})
	go ServeBoth(l, srv, other, "/git/", "/github-api/")

	// 許可していない宛先: dial-check より前の allowlist で 403 になる (実際に繋ぎに行かない)。
	status := dialLine(t, l.Addr().String(), "CONNECT not-allowed.invalid:443 HTTP/1.1")
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("status = %q", status)
	}
	mu.Lock()
	got := auditBuf.String()
	mu.Unlock()
	if !strings.Contains(got, `"event":"deny"`) || !strings.Contains(got, "not-allowed.invalid:443") {
		t.Fatalf("監査に、CONNECT の拒否が出ていない: %q", got)
	}
}

// TestServeBothDispatchesHTTP は、git・github-api の接頭辞に一致する GET・POST が、other (http.Handler) に届くことを確かめる。
func TestServeBothDispatchesHTTP(t *testing.T) {
	var got []string
	var mu sync.Mutex
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.RequestURI)
		mu.Unlock()
		w.WriteHeader(299)
	})
	connect := New(Config{Allow: nil, Audit: io.Discard})
	l := newTestListener(t)
	go ServeBoth(l, connect, other, "/git/", "/github-api/")

	cases := []string{
		"GET /git/github.com/o/r.git/info/refs?service=git-upload-pack HTTP/1.1",
		"POST /git/github.com/o/r.git/git-receive-pack HTTP/1.1",
		"POST /github-api/repos/o/r/pulls HTTP/1.1",
	}
	for _, line := range cases {
		status := dialLine(t, l.Addr().String(), line+"\r\nHost: x\r\nContent-Length: 0")
		if !strings.HasPrefix(status, "HTTP/1.1 299") {
			t.Fatalf("%s: status = %q", line, status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(cases) {
		t.Fatalf("届いた要求 = %v", got)
	}
	for i, line := range cases {
		want := strings.TrimSuffix(strings.Split(line, " HTTP/")[0], "")
		if got[i] != want {
			t.Errorf("[%d] got %q, want %q", i, got[i], want)
		}
	}
}

// TestServeBothRejectsOther は、接頭辞に一致しない method・path や、CONNECT でも GET/POST でもない method を、
// どちらのハンドラにも渡さず、400 で閉じることを確かめる。
func TestServeBothRejectsOther(t *testing.T) {
	connect := New(Config{Audit: io.Discard})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("呼ばれるはずがない: %s %s", r.Method, r.RequestURI)
	})
	go ServeBoth(l, connect, other, "/git/", "/github-api/")

	// "/git/../etc" のような、接頭辞の後の中身までは、ここでは見ない (文字列の前方一致だけ。".." などの意味は、
	// 渡した先の git.ParseRoute の仕事。PR ① の route_test.go で確かめている)。
	for _, line := range []string{
		"GET /other/path HTTP/1.1\r\nHost: x",
		"GET /gitx HTTP/1.1\r\nHost: x", // 接頭辞 "/git/" に前方一致しない (紛らわしい名前)
		"PUT /git/github.com/o/r.git/git-receive-pack HTTP/1.1\r\nHost: x\r\nContent-Length: 0",
		"DELETE /github-api/repos/o/r/pulls HTTP/1.1\r\nHost: x",
		"GARBAGE",
		"",
	} {
		status := dialLine(t, l.Addr().String(), line)
		if !strings.HasPrefix(status, "HTTP/1.1 400") {
			t.Errorf("%q: status = %q", line, status)
		}
	}
}

func TestHasAnyPrefix(t *testing.T) {
	if hasAnyPrefix("/git/x", nil) {
		t.Error("prefixes が空なら、何にも一致しないはず")
	}
	if hasAnyPrefix("/git/x", []string{}) {
		t.Error("prefixes が空 (nil でない) でも、何にも一致しないはず")
	}
	if !hasAnyPrefix("/git/x", []string{"/other/", "/git/"}) {
		t.Error("一致するはず")
	}
	if hasAnyPrefix("/gitx", []string{"/git/"}) {
		t.Error("接頭辞でない一致 (/gitx が /git/ を含む) を、一致とした")
	}
}

// TestServeBothLongLine は、最初の行が上限を超えるとき、パニックせず 400 で閉じることを確かめる。
func TestServeBothLongLine(t *testing.T) {
	connect := New(Config{Audit: io.Discard})
	l := newTestListener(t)
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("呼ばれるはずがない") })
	go ServeBoth(l, connect, other, "/git/")

	status := dialLine(t, l.Addr().String(), "GET /git/"+strings.Repeat("a", maxSniffLine*2)+" HTTP/1.1")
	if !strings.HasPrefix(status, "HTTP/1.1 400") {
		t.Fatalf("status = %q", status)
	}
}

// TestServeBothConcurrent は、CONNECT と git/HTTP の要求を、同じ listener に混ぜて多数投げても、取り違えないことを確かめる。
func TestServeBothConcurrent(t *testing.T) {
	connect := New(Config{Allow: []string{"allowed.invalid:443"}, Audit: io.Discard})
	var hits atomicInt
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.add(1); w.WriteHeader(200) })
	l := newTestListener(t)
	go ServeBoth(l, connect, other, "/git/")

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				status := dialLine(t, l.Addr().String(), "CONNECT not-allowed.invalid:443 HTTP/1.1")
				if !strings.HasPrefix(status, "HTTP/1.1 403") {
					t.Errorf("connect: status = %q", status)
				}
			} else {
				status := dialLine(t, l.Addr().String(), "GET /git/x HTTP/1.1\r\nHost: x\r\nContent-Length: 0")
				if !strings.HasPrefix(status, "HTTP/1.1 200") {
					t.Errorf("git: status = %q", status)
				}
			}
		}(i)
	}
	wg.Wait()
	if got := hits.get(); got != 20 {
		t.Fatalf("git ハンドラは 20 回呼ばれるはず: %d", got)
	}
}

// FuzzDispatch は、任意の最初の行で、dispatch がパニックしないことを確かめる。
func FuzzDispatch(f *testing.F) {
	for _, s := range []string{"CONNECT x:443 HTTP/1.1", "GET /git/x HTTP/1.1", "GARBAGE", "", "CONNECT", "GET /github-api/x", strings.Repeat("A", 5000)} {
		f.Add(s)
	}
	connect := New(Config{Audit: io.Discard})
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	f.Fuzz(func(t *testing.T, line string) {
		server, client := net.Pipe()
		done := make(chan struct{})
		go func() {
			dispatch(server, connect, &http.Server{Handler: other}, []string{"/git/", "/github-api/"})
			close(done)
		}()
		client.SetDeadline(time.Now().Add(2 * time.Second))
		client.Write([]byte(line + "\r\n"))
		io.Copy(io.Discard, client)
		client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
}

// writerFunc は、関数を io.Writer にする。
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// atomicInt は、テストだけで使う、単純な排他カウンタ。
type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) add(d int) {
	a.mu.Lock()
	a.n += d
	a.mu.Unlock()
}
func (a *atomicInt) get() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

//go:build linux

package main

import (
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ctlPair は、control の socketpair (ホスト側・init 側)。
func ctlPair(t *testing.T) (host, initSide *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "ctl")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c.(*net.UnixConn)
	}
	return mk(fds[0]), mk(fds[1])
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

// sendFDs は、1 バイトと fds (SCM_RIGHTS) を control に送る。
func sendFDs(t *testing.T, c *net.UnixConn, fds ...int) {
	t.Helper()
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	if _, _, err := c.WriteMsgUnix([]byte{0}, oob, nil); err != nil {
		t.Fatal(err)
	}
}

func mustSocketpair(t *testing.T, typ int) (int, int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, typ|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fds[0], fds[1]
}

// 規約に合う fd (unix domain の stream socket 1 個) だけが、接続として渡る。規約外 (0 個・2 個以上・oob の上限超え・socket でない・stream でない・unix でない)
// は、受けた fd を全部閉じ (fd が増えない)、control の読みは続く。
func TestServeControlAcceptsOnlyWellFormed(t *testing.T) {
	host, init_ := ctlPair(t)
	accepted := make(chan net.Conn, 16)
	done := make(chan error, 1)
	go func() { done <- serveControl(init_, func(c net.Conn) { accepted <- c }) }()
	base := openFDs(t)

	closeAll := func(fds ...int) {
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}
	// 規約外: どれも、受け付けられない。
	a1, a2 := mustSocketpair(t, syscall.SOCK_STREAM)
	b1, b2 := mustSocketpair(t, syscall.SOCK_STREAM)
	sendFDs(t, host, a2, b2) // 2 個
	closeAll(a1, a2, b1, b2)

	sendFDs(t, host) // 0 個 (余計なバイト)

	var pfds [2]int
	if err := syscall.Pipe2(pfds[:], syscall.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	sendFDs(t, host, pfds[0]) // socket ではない
	closeAll(pfds[0], pfds[1])

	d1, d2 := mustSocketpair(t, syscall.SOCK_DGRAM)
	sendFDs(t, host, d2) // stream ではない
	closeAll(d1, d2)

	tcp, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	sendFDs(t, host, tcp) // unix ではない
	syscall.Close(tcp)

	many := make([]int, controlMaxFDs+2)
	var keep []int
	for i := range many {
		x, y := mustSocketpair(t, syscall.SOCK_STREAM)
		many[i] = y
		keep = append(keep, x, y)
	}
	sendFDs(t, host, many...) // oob の上限超え (MSG_CTRUNC)
	closeAll(keep...)

	// 規約に合う 1 個は、そのあとでも通る (control の読みが続いている)。
	g1, g2 := mustSocketpair(t, syscall.SOCK_STREAM)
	sendFDs(t, host, g2)
	syscall.Close(g2)
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(testTimeout):
		t.Fatal("規約に合う fd が、渡らない")
	}
	select {
	case c := <-accepted:
		c.Close()
		t.Fatal("規約外の fd が、接続として渡った")
	case <-time.After(300 * time.Millisecond):
	}
	syscall.Close(g1)

	// 受けた fd は、全部閉じられている (fd が、規約外の分だけ増えていない)。
	deadline := time.Now().Add(testTimeout)
	for openFDs(t) > base && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := openFDs(t); n > base {
		t.Errorf("fd が漏れた: %d → %d", base, n)
	}

	host.Close() // EOF で、serveControl は戻る
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("EOF で戻った error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("control の EOF で戻らない")
	}
}

// 受けた fd は close-on-exec (子プロセスに漏れない)。Go の ReadMsgUnix が MSG_CMSG_CLOEXEC を付けることに、serveControl は頼っている。
func TestControlReceivedFDIsCloexec(t *testing.T) {
	host, init_ := ctlPair(t)
	a, b := mustSocketpair(t, syscall.SOCK_STREAM)
	defer syscall.Close(a)
	sendFDs(t, host, b)
	syscall.Close(b)
	oob := make([]byte, syscall.CmsgSpace(4*controlMaxFDs))
	_, oobn, _, _, err := init_.ReadMsgUnix(make([]byte, 1), oob)
	if err != nil {
		t.Fatal(err)
	}
	fds := parseRights(oob[:oobn])
	if len(fds) != 1 {
		t.Fatalf("fd = %v", fds)
	}
	defer syscall.Close(fds[0])
	flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fds[0]), syscall.F_GETFD, 0)
	if e != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Errorf("受けた fd の FD_CLOEXEC が立っていない: flags=%d errno=%v", flags, e)
	}
}

// 通し (同じプロセスの中): ホストの http.Client → control → init の relay → 偽の上流。偽造した認証は上流に届かず、応答が返り、同時の要求も通る。
func TestRelayClientHTTPEndToEnd(t *testing.T) {
	var seenAuth atomic.Value
	up := newRelayUpstream(t, func(c net.Conn, req string) {
		for _, l := range strings.Split(req, "\r\n") {
			if strings.HasPrefix(l, "Authorization: ") {
				seenAuth.Store(l)
			}
		}
		okResponse("ok")(c, req)
	})
	host, init_ := ctlPair(t)
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	go serveControl(init_, r.accept)
	hc := newRelayClient(host).HTTPClient()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := newRequest("POST", "http://relay.invalid/api/session", `{"a":1}`)
			req.Header.Set("Authorization", "Bearer forged")
			resp, err := hc.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(b) != "ok" {
				t.Errorf("status=%d body=%q", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	if got, _ := seenAuth.Load().(string); got != "Authorization: "+basicAuth("TOKEN") {
		t.Errorf("上流が受けた認証 = %q", got)
	}
	if n := len(up.requests()); n != 20 {
		t.Errorf("上流が受けた要求 = %d 個, want 20", n)
	}
}

// fd の洪水: 要求を書かない fd を大量に送っても、init が同時に持つ fd は、上限で頭打ちになる (枠の超過は、すぐ閉じる)。
func TestServeControlFDFlood(t *testing.T) {
	up := newRelayUpstream(t, okResponse("x"))
	host, init_ := ctlPair(t)
	r := newRequestRelay(up.port(), func() string { return "TOKEN" })
	go serveControl(init_, r.accept)
	base := openFDs(t)
	var locals []net.Conn
	rc := newRelayClient(host)
	for i := 0; i < 4*relayMaxInflight; i++ {
		c, err := rc.Dial()
		if err != nil {
			t.Fatal(err)
		}
		locals = append(locals, c) // 要求を書かない
	}
	time.Sleep(300 * time.Millisecond)
	// ホスト側の複製 (locals) と、init 側 (上限つき) の分しか、増えない。
	if n := openFDs(t); n > base+len(locals)+relayMaxInflight+8 {
		t.Errorf("fd が増えすぎた: %d → %d (host 側 %d + 上限 %d)", base, n, len(locals), relayMaxInflight)
	}
	for _, c := range locals {
		c.Close()
	}
	deadline := time.Now().Add(testTimeout)
	for openFDs(t) > base+4 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := openFDs(t); n > base+4 {
		t.Errorf("閉じた後も fd が残る: %d → %d", base, n)
	}
}

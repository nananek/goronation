//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
)

// control の規約 (ADR 0019・0023): ホストが、要求ごとに socketpair を作り、片端を、control (init の標準入力の socket) に、
// 1 バイトのデータと SCM_RIGHTS の 1 個の fd で送る。init は、1 バイトずつ受けて (fd を、境界の正しい 1 バイトに結びつける)、
// fd を要求の処理に渡す。規約に外れるもの (fd が 0 個・2 個以上・socket でないもの・stream でないもの・unix でないもの) は、
// 受けた fd を全部閉じる。
const (
	// controlMaxFDs は、1 回の受信で受け付ける fd の数の上限 (oob の大きさ)。超えた分は、kernel が閉じ (MSG_CTRUNC)、受けた分も全部閉じる。
	controlMaxFDs = 4
	// controlWriteTimeout は、ホスト側が control に書く 1 回の上限。
	controlWriteTimeout = 5 * time.Second
)

// serveControl は、ctl から fd を受け続け、検査を通った fd を、接続として accept に渡す。ctl が閉じられる (EOF)・読めなくなると、戻る。
// 受けた fd は close-on-exec (Go の ReadMsgUnix は、Linux で MSG_CMSG_CLOEXEC を付ける。ここでも、受けた fd の FD_CLOEXEC を確かめて、
// 立っていなければ立てる)。
func serveControl(ctl *net.UnixConn, accept func(net.Conn)) error {
	oob := make([]byte, syscall.CmsgSpace(4*controlMaxFDs))
	buf := make([]byte, 1)
	for {
		n, oobn, flags, _, err := ctl.ReadMsgUnix(buf, oob)
		fds := parseRights(oob[:oobn])
		if err != nil || (n == 0 && len(fds) == 0) {
			closeFDs(fds)
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if flags&syscall.MSG_CTRUNC != 0 || len(fds) != 1 {
			closeFDs(fds) // 規約外: 0 個 (余計なバイト)・2 個以上・切り詰め
			continue
		}
		conn, err := connFromFD(fds[0])
		if err != nil {
			continue
		}
		accept(conn)
	}
}

// parseRights は、oob の制御メッセージのうち、SCM_RIGHTS の fd を、すべて返す (ほかの型は無視する)。
func parseRights(oob []byte) []int {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var fds []int
	for i := range msgs {
		if got, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
			fds = append(fds, got...)
		}
	}
	return fds
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}

// connFromFD は、fd が、unix domain の stream socket のときだけ、net.Conn にする (fd の所有権は、成功・失敗とも、ここで引き取る)。
func connFromFD(fd int) (net.Conn, error) {
	syscall.CloseOnExec(fd)
	t, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || t != syscall.SOCK_STREAM {
		syscall.Close(fd)
		return nil, errors.New("stream socket ではない")
	}
	sa, err := syscall.Getsockname(fd)
	if _, ok := sa.(*syscall.SockaddrUnix); err != nil || !ok {
		syscall.Close(fd)
		return nil, errors.New("unix domain socket ではない")
	}
	f := os.NewFile(uintptr(fd), "relay-request")
	defer f.Close() // FileConn は複製を作る
	return net.FileConn(f)
}

// relayClient は、ホスト側: control (chat セッションの、init の標準入力の socketpair のホスト側) へ、要求ごとの socketpair の片端を送る。
// 1 つの接続は 1 つの HTTP 要求だけ (init が Connection: close を強制する)。
type relayClient struct {
	mu  sync.Mutex // 1 バイト + fd の送信を、ほかの送信と混ぜない
	ctl *net.UnixConn
}

func newRelayClient(ctl *net.UnixConn) *relayClient { return &relayClient{ctl: ctl} }

// Dial は、新しい socketpair を作り、片端を init に送り、もう片端を、要求を書く接続として返す。
func (c *relayClient) Dial() (net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	local, remote := os.NewFile(uintptr(fds[0]), "relay-local"), os.NewFile(uintptr(fds[1]), "relay-remote")
	defer local.Close()  // FileConn は複製を作る
	defer remote.Close() // 送った後は、この複製は要らない (閉じないと、相手が閉じても EOF にならない)
	conn, err := net.FileConn(local)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.ctl.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
	_, _, err = c.ctl.WriteMsgUnix([]byte{0}, syscall.UnixRights(int(remote.Fd())), nil)
	c.mu.Unlock()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// HTTPClient は、Dial だけで繋ぐ http.Client。宛先 (URL のホスト) は無視され、init が上流を決める。接続の再利用は止める
// (1 つの接続は 1 つの要求)。
func (c *relayClient) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:       func(_ context.Context, _, _ string) (net.Conn, error) { return c.Dial() },
		DisableKeepAlives: true,
	}}
}

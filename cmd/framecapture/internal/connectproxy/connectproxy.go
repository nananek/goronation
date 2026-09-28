package connectproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// 既定値。Server の対応するフィールドで上書きできる (テストが縮める)。
const (
	DefaultHeaderTimeout = 5 * time.Second
	DefaultDialTimeout   = 10 * time.Second
	DefaultIdleTimeout   = 5 * time.Minute
)

// maxHeaderBytes は、CONNECT のリクエスト行・ヘッダー 1 行に許す上限。
const maxHeaderBytes = 8 << 10

// Server は、CONNECT だけを受ける、最小限のプロキシ。New で作る。ゼロ値は使わない。
//
// 許可は、New に渡した対応表の完全一致だけ (ワイルドカードも、名前解決も無い)。表に無い宛先・CONNECT
// 以外のメソッドは拒否する。dial する IP の検査 (SSRF 対策) はしない: 対応表にある実アドレスを、
// 無条件に信頼して dial する。
type Server struct {
	allow map[string]netip.AddrPort // key は小文字化した "host:port"

	// HeaderTimeout・DialTimeout・IdleTimeout は、New が既定値 (Default*) で埋める。
	HeaderTimeout time.Duration
	DialTimeout   time.Duration
	IdleTimeout   time.Duration

	// Logf は、nil でなければ、accept・deny・dial 失敗のたびに 1 行呼ばれる (監査ログではなく、
	// デバッグ用の軽いフック)。
	Logf func(format string, args ...any)

	// dial は、テストだけが差し替える (既定は実際に TCP で繋ぐ)。
	dial func(ctx context.Context, network, address string) (net.Conn, error)

	mu        sync.Mutex
	closed    bool
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

// New は、allow (クライアントが CONNECT 行に書く仮想の "host:port" → 実際に dial する実アドレス) だけを
// 許可する Server を作る。host は大文字小文字を区別しない。
func New(allow map[string]netip.AddrPort) *Server {
	lower := make(map[string]netip.AddrPort, len(allow))
	for k, v := range allow {
		lower[strings.ToLower(k)] = v
	}
	return &Server{
		allow:         lower,
		HeaderTimeout: DefaultHeaderTimeout,
		DialTimeout:   DefaultDialTimeout,
		IdleTimeout:   DefaultIdleTimeout,
		dial:          (&net.Dialer{}).DialContext,
		listeners:     map[net.Listener]struct{}{},
		conns:         map[net.Conn]struct{}{},
	}
}

// Serve は、l で接続を受けて、CONNECT を処理する。l が閉じられる (Close 経由を含む) と nil を返す
// (呼び手が閉じた結果なので、error として扱わない)。
func (s *Server) Serve(l net.Listener) error {
	defer l.Close()
	if !s.addListener(l) {
		return nil // すでに Close 済み
	}
	defer s.removeListener(l)
	for {
		c, err := l.Accept()
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !s.addConn(c) {
			c.Close()
			continue
		}
		go func() {
			defer s.wg.Done()
			defer s.removeConn(c)
			s.handle(c)
		}()
	}
}

// Close は、待ち受け中の listener と、進行中の接続をすべて閉じ、handle が終わるのを待つ。
// 何度呼んでもよい。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return nil
	}
	s.closed = true
	ls := make([]net.Listener, 0, len(s.listeners))
	for l := range s.listeners {
		ls = append(ls, l)
	}
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, l := range ls {
		l.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) addListener(l net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.listeners[l] = struct{}{}
	return true
}

func (s *Server) removeListener(l net.Listener) {
	s.mu.Lock()
	delete(s.listeners, l)
	s.mu.Unlock()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// addConn は、c を追跡し、wg に数える。Close 済みなら false を返す (wg.Add を Close の Wait と
// 競合させないため、mu の中で行う)。
func (s *Server) addConn(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) removeConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// handle は、1 本の接続を、CONNECT なら許可表に従って中継し、それ以外は拒否する。
func (s *Server) handle(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(s.HeaderTimeout))
	br := bufio.NewReaderSize(c, maxHeaderBytes)

	target, status := s.readConnect(br)
	if status != 0 {
		s.logf("connectproxy: deny target=%q status=%d", target, status)
		if status > 0 {
			writeStatus(c, status)
		}
		return
	}
	real := s.allow[target] // readConnect が存在を確かめ済み

	ctx, cancel := context.WithTimeout(context.Background(), s.DialTimeout)
	up, err := s.dial(ctx, "tcp", real.String())
	cancel()
	if err != nil {
		s.logf("connectproxy: dial %s (target=%q) 失敗: %v", real, target, err)
		writeStatus(c, 502)
		return
	}
	defer up.Close()

	c.SetReadDeadline(time.Time{}) // ヘッダーの期限を解除。以降は IdleTimeout に任せる
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	s.logf("connectproxy: allow target=%q -> %s", target, real)
	relay(c, br, up, s.IdleTimeout)
}

// readConnect は、CONNECT のリクエスト行・ヘッダーを読み切る。成功すれば、対象 (小文字化した
// "host:port"。許可表にもあることを確かめ済み) と status=0 を返す。失敗・不許可なら、status に
// 書くべき HTTP ステータス (400・403・405) か、応答を書かずに閉じるべきことを示す -1 を返す。
func (s *Server) readConnect(br *bufio.Reader) (target string, status int) {
	line, err := readLine(br)
	if err != nil {
		return "", -1
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || (parts[2] != "HTTP/1.1" && parts[2] != "HTTP/1.0") {
		return "", 400
	}
	if parts[0] != "CONNECT" {
		return "", 405
	}
	target = strings.ToLower(parts[1])

	for {
		h, err := readLine(br)
		if err != nil {
			return "", -1
		}
		if h == "" {
			break
		}
	}
	if _, ok := s.allow[target]; !ok {
		return target, 403
	}
	return target, 0
}

// readLine は、CRLF で終わる 1 行を、CRLF を除いて返す。maxHeaderBytes を超える・CRLF で終わらない
// 行は error。
func readLine(br *bufio.Reader) (string, error) {
	b, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("connectproxy: 行が長すぎる")
		}
		return "", err
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return "", fmt.Errorf("connectproxy: CRLF で終わっていない")
	}
	return string(b[:len(b)-2]), nil
}

var statusText = map[int]string{
	400: "Bad Request",
	403: "Forbidden",
	405: "Method Not Allowed",
	502: "Bad Gateway",
}

func writeStatus(c net.Conn, code int) {
	c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nConnection: close\r\n\r\n", code, statusText[code])
}

// relay は、client (先読みしたバイトは cr に残っている) と up の間を、両方向に中継する。片方向の
// EOF は、その向きの書き込み側だけを閉じる (半クローズ。CloseWrite が無い conn は、両方を閉じる)。
// EOF 以外の error・idle の期限は、両方を閉じる。idle は、両方向をあわせた無通信の期限で、どちらかが
// 動いている間は延びる (egress.relay と同じ設計)。
func relay(client net.Conn, cr io.Reader, up net.Conn, idle time.Duration) {
	var once sync.Once
	stop := func() {
		once.Do(func() {
			client.Close()
			up.Close()
		})
	}
	timer := time.AfterFunc(idle, stop)
	defer timer.Stop()
	touch := func() { timer.Reset(idle) }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pipe(up, cr, touch, stop)
	}()
	go func() {
		defer wg.Done()
		pipe(client, up, touch, stop)
	}()
	wg.Wait()
	stop()
}

// pipe は、src から dst へ、EOF まで写す。読み書きのたびに touch を呼ぶ。
func pipe(dst net.Conn, src io.Reader, touch, abort func()) {
	buf := make([]byte, 32<<10)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			touch()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				abort()
				return
			}
			touch()
		}
		if rerr != nil {
			if rerr != io.EOF {
				abort()
				return
			}
			if cw, ok := dst.(interface{ CloseWrite() error }); !ok || cw.CloseWrite() != nil {
				abort()
			}
			return
		}
	}
}

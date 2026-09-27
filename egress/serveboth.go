package egress

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxSniffLine は、接続の最初の行 (method と request-target) を読むための上限。CONNECT の宛先も、git・PR の path も、
// これに収まる (Config.MaxHeaderBytes とは別。1 行の read だけで、ヘッダは読まない)。
const maxSniffLine = 4 << 10

// ServeBoth は、l で、CONNECT (connect) と、それ以外の HTTP (git smart-HTTP・PR 作成のエンドポイントなど。http.Handler の
// other) を、同じ listener で受ける。各接続の最初の行だけを読んで振り分け、connect の既存の処理 (readRequest 以降) は
// 変えない: 読んだ行は、そのまま接続の先頭に戻し (二重に読ませない)、connect.accepted に渡す。
// method が CONNECT なら connect へ、GET・POST で request-target が prefixes のどれかで始まれば other (1 接続に 1 つの
// http.Server) へ、それ以外は 400 で閉じる。戻るのは、l が閉じたとき (Close は、呼び手が l に対して行う)。
func ServeBoth(l net.Listener, connect *Server, other http.Handler, prefixes ...string) error {
	httpSrv := &http.Server{Handler: other}
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go dispatch(c, connect, httpSrv, prefixes)
	}
}

// dispatch は、1 本の接続の、最初の行を読んで振り分ける。
func dispatch(c net.Conn, connect *Server, httpSrv *http.Server, prefixes []string) {
	c.SetReadDeadline(time.Now().Add(connect.cfg.HeaderTimeout))
	br := bufio.NewReaderSize(c, maxSniffLine)
	line, err := br.ReadSlice('\n')
	c.SetReadDeadline(time.Time{})
	if err != nil {
		// 上限を超えた・読めなかった・時間切れ: CONNECT の readRequest と同じ理由で断る (400・431・408 は区別しない。1 行も
		// 読めない接続に、理由を返す価値は薄い)。
		writeStatus(c, 400)
		c.Close()
		return
	}
	// 読んだ行を、接続の先頭に戻す (br に残った分は、そのまま後ろに続く)。
	pc := &prefixedConn{Conn: c, r: io.MultiReader(bytes.NewReader(line), br)}
	method, target, ok := requestLineParts(line)
	switch {
	case method == "CONNECT":
		connect.accepted(pc)
	case ok && (method == "GET" || method == "POST") && hasAnyPrefix(target, prefixes):
		serveOneConn(pc, httpSrv)
	default:
		writeStatus(pc, 400)
		pc.Close()
	}
}

// requestLineParts は、"METHOD target HTTP/1.x\r\n" から、method と target を取り出す。version の検査は、渡した先
// (connect・http.Server) が、それぞれの流儀で行う。
func requestLineParts(line []byte) (method, target string, ok bool) {
	parts := strings.SplitN(strings.TrimRight(string(line), "\r\n"), " ", 3)
	if len(parts) != 3 {
		return parts[0], "", false
	}
	return parts[0], parts[1], true
}

// hasAnyPrefix は、s が、prefixes のどれかで始まるか。
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// prefixedConn は、最初に読んでしまった行 (と、bufio が余分に読んだ分) を、Read の出どころにする net.Conn。
// Write・Close などは、埋め込んだ net.Conn (元の接続) に任せる。
type prefixedConn struct {
	net.Conn
	r io.Reader
}

func (p *prefixedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// oneConnListener は、1 本の net.Conn だけを Accept で返し、以後は net.ErrClosed を返す net.Listener
// (http.Server.Serve に、その接続だけを渡すため)。
type oneConnListener struct {
	c    net.Conn
	once sync.Once
}

func (o *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	o.once.Do(func() { c = o.c })
	if c == nil {
		return nil, net.ErrClosed
	}
	return c, nil
}

func (o *oneConnListener) Close() error   { return nil }
func (o *oneConnListener) Addr() net.Addr { return o.c.LocalAddr() }

// serveOneConn は、c を、httpSrv (http.Handler は共有。呼び手ごとに新しい *http.Server を作らない) に、1 回だけ渡す。
// httpSrv.Serve は、この接続を扱う goroutine を起こしてから、2 回目の Accept で ErrClosed を見て、すぐに戻る
// (接続自体の処理は、その goroutine が、c が閉じるまで続ける)。
func serveOneConn(c net.Conn, httpSrv *http.Server) {
	httpSrv.Serve(&oneConnListener{c: c})
}

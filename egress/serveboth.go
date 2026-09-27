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
//
// other 側の httpSrv は ServeBoth が内部で作るため、呼び手には後から ReadHeaderTimeout 等を設定する手段が無い。
// connect.cfg (HeaderTimeout・IdleTimeout・MaxConns) を、ここでそのまま other 経路にも適用する
// (newOtherHTTPServer を見よ)。
func ServeBoth(l net.Listener, connect *Server, other http.Handler, prefixes ...string) error {
	httpSrv := newOtherHTTPServer(connect, other)
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go dispatch(c, connect, httpSrv, prefixes)
	}
}

// newOtherHTTPServer は、other 用の *http.Server を、connect.cfg に合わせて作る。ServeBoth 本体と、
// dispatch を直に呼ぶテスト (FuzzDispatch) の両方が、この 1 か所を使う (semaphore の acquire (dispatch 側)
// と release (ここの ConnState) の対を、生成のたびに書き写さない)。
//
//   - ReadHeaderTimeout・IdleTimeout: dispatch が最初の行を読んだ後に SetReadDeadline(time.Time{}) で
//     解除する締め切りを、net/http がヘッダを読み始める時点で改めて架け直す。
//   - Handler は stallGuardBody で包み、本文 (r.Body) の読み取りにも締め切りを持たせる (本文フェーズの
//     Slowloris・トリクル対策)。主たる防御は絶対の締め切り (maxBodyReadDuration。一度だけ決め、以後
//     延長しない) で、進捗ベースの延長 (IdleTimeout) は、完全に止まった接続を早めに切る応答性のために
//     残す (どちらが早く来ても、そちらで切れる)。
//   - ConnState: dispatch が connect.sem から取った 1 枠を、この接続が本当に終わった (StateClosed・
//     StateHijacked) ときに返す。serveOneConn の httpSrv.Serve は goroutine を起こすとすぐ戻るため、
//     その戻りを「終わった」合図にはできない。
func newOtherHTTPServer(connect *Server, other http.Handler) *http.Server {
	return &http.Server{
		Handler:           stallGuardBody(other, connect.cfg),
		ReadHeaderTimeout: connect.cfg.HeaderTimeout,
		IdleTimeout:       connect.cfg.IdleTimeout,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				<-connect.sem
			}
		},
	}
}

// stallGuardBody は、h に渡す前に、要求の本文 (r.Body) を、締め切りを持つ io.ReadCloser (stallBody) に
// 差し替える。呼ばれるのはハンドラの中 (つまり net/http 自身の ReadHeaderTimeout が、ヘッダを読み終えて
// 役目を終えた後) だけなので、ヘッダの読み取りには影響しない。
//
// 締め切りは 2 段構え: (1) 絶対の締め切り (maxBodyReadDuration。本文を読み始めた時点で一度だけ決め、以後
// 延長しない) を主たる防御にする。「読むたびに延ばす」進捗ベースの検知だけでは、締め切りより短い間隔で
// 1 バイトずつ送るトリクル (実効スループットがほぼ 0) を原理的に防げない (どんなに細い進捗でも延びる設計
// である以上、当然の帰結)。(2) 進捗ベースの締め切り (IdleTimeout。読めるたびに、そこから先に延びる) は、
// 完全に止まった接続を、絶対の締め切りより早く切るための応答性の補助として残す。実際に効く締め切りは、
// 読むたびに、この 2 つの早い方 (min) になる。
//
// cfg.IdleTimeout が 0 以下 (Config が壊れている異常系) なら、締め切りを 0 (= 即座に期限切れ) にして
// しまわないよう、何もしない。
func stallGuardBody(h http.Handler, cfg Config) http.Handler {
	if cfg.IdleTimeout <= 0 {
		return h
	}
	absolute := maxBodyReadDuration(cfg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &stallBody{
				ReadCloser: r.Body,
				rc:         http.NewResponseController(w),
				stall:      cfg.IdleTimeout,
				deadline:   time.Now().Add(absolute),
			}
		}
		h.ServeHTTP(w, r)
	})
}

// stallBody は、Read のたびに (net/http の ResponseController 経由で) 接続の読み取り締め切りを架け直す
// io.ReadCloser。架け直す先は、「今から stall 先」と、固定の絶対の締め切り (deadline) の、早い方
// (min)。deadline を過ぎた後は、常に deadline 自身が選ばれる (それより先には、決して延びない)。
type stallBody struct {
	io.ReadCloser
	rc       *http.ResponseController
	stall    time.Duration
	deadline time.Time
}

func (b *stallBody) Read(p []byte) (int, error) {
	d := time.Now().Add(b.stall)
	if d.After(b.deadline) {
		d = b.deadline
	}
	b.rc.SetReadDeadline(d)
	return b.ReadCloser.Read(p)
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
	method, target := requestLineParts(line)
	switch {
	case method == "CONNECT":
		connect.accepted(pc)
	case (method == "GET" || method == "POST") && hasAnyPrefix(target, prefixes):
		// CONNECT (accepted 内) と同じ connect.sem を、同時接続数の上限として共有する。枠が無ければ 503 で
		// 断る。取った枠は、newOtherHTTPServer の ConnState (StateClosed) が、この接続が実際に終わった
		// ときに返す (ここで解放しない: serveOneConn はすぐ戻るため)。
		select {
		case connect.sem <- struct{}{}:
		default:
			connect.refuse(pc, connect.nextID.Add(1), deny(503, reasonBusy, ""))
			return
		}
		serveOneConn(pc, httpSrv)
	default:
		writeStatus(pc, 400)
		pc.Close()
	}
}

// requestLineParts は、"METHOD target HTTP/1.x\r\n" から、method と target を取り出す。3 つに分かれない (target・
// version が無い) 行は、target を空で返す (空の target は、prefixes のどれにも前方一致しないので、hasAnyPrefix が
// 断る。「3 つに分かれたか」を、別に見る必要が無い)。version の検査は、渡した先 (connect・http.Server) が、それぞれの
// 流儀で行う。
func requestLineParts(line []byte) (method, target string) {
	parts := strings.SplitN(strings.TrimRight(string(line), "\r\n"), " ", 3)
	if len(parts) == 3 {
		target = parts[1]
	}
	return parts[0], target
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

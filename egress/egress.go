package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed は、Close された Server の Serve が返す error。
var ErrClosed = errors.New("egress: server は閉じられている")

// ErrConfig は、Config が不正なときに Serve が返す error (errors.Is で判定する)。
var ErrConfig = errors.New("egress: 設定が不正")

// 上限の既定値。Config の 0 は、これになる。
const (
	defaultMaxConns       = 128
	defaultMaxHeaderBytes = 8 << 10
	defaultHeaderTimeout  = 10 * time.Second
	defaultDialTimeout    = 10 * time.Second
	defaultIdleTimeout    = 5 * time.Minute
)

const (
	// readBufSize は、ヘッダを読む bufio の大きさ。ヘッダの 1 行は、これを超えられない。
	readBufSize = 4 << 10
	// maxAddrs は、1 つの名前について、順に試す IP の数の上限。
	maxAddrs = 8
	// statusTimeout は、拒否の応答 (と 200) を書く期限。
	statusTimeout = 3 * time.Second
)

// Resolver は、許可した名前を IP に解決する。*net.Resolver が満たす。
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Config は、Server の設定。数値の 0 は既定値になり、負の値は ErrConfig になる。
type Config struct {
	// Allow は、許可する宛先 "host:port" の一覧。完全一致だけで、ワイルドカードも IP リテラルも書けない。
	Allow []string
	// Audit は、監査の出力先 (必須)。1 回の CONNECT につき、JSON を 1 行書く。
	Audit io.Writer
	// Resolver は、名前の解決に使う。nil なら net.DefaultResolver。
	Resolver Resolver
	// MaxConns は、同時に扱う接続 (ヘッダを読んでいる間を含む) の上限。既定 128。
	MaxConns int
	// MaxHeaderBytes は、リクエストのヘッダ全体の上限。既定 8 KiB。
	MaxHeaderBytes int
	// HeaderTimeout は、ヘッダを読み終えるまでの期限。既定 10 秒。
	HeaderTimeout time.Duration
	// DialTimeout は、名前解決と接続の期限 (合計)。既定 10 秒。
	DialTimeout time.Duration
	// IdleTimeout は、両方向とも通信が無い時間の上限。既定 5 分。
	IdleTimeout time.Duration
}

// Server は、CONNECT プロキシ。New で作る。
type Server struct {
	cfg      Config
	allow    map[string]struct{}
	resolver Resolver
	err      error // 設定の error。Serve が返す

	// forbid と dial は、パッケージ内のテストだけが差し替える。
	// 既定は、実際に接続する IP を Control で検査する dialer (dial の Control は forbid を呼ぶ)。
	forbid func(netip.Addr) bool
	dial   func(ctx context.Context, network, address string) (net.Conn, error)

	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	nextID atomic.Uint64
	wg     sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}

	auditMu sync.Mutex
}

// New は、cfg から Server を作る。cfg が不正でも失敗せず、Serve が ErrConfig を返す。
func New(cfg Config) *Server {
	s := &Server{
		cfg:       cfg,
		forbid:    forbidden,
		listeners: map[net.Listener]struct{}{},
		conns:     map[net.Conn]struct{}{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.dial = (&net.Dialer{Control: s.control}).DialContext
	s.err = s.setup()
	s.sem = make(chan struct{}, max(s.cfg.MaxConns, 1))
	return s
}

// setup は、既定値を入れ、Allow を検証して、許可の表を作る。
func (s *Server) setup() error {
	c := &s.cfg
	if c.Audit == nil {
		return fmt.Errorf("%w: Audit が nil", ErrConfig)
	}
	if c.MaxConns < 0 || c.MaxHeaderBytes < 0 || c.HeaderTimeout < 0 || c.DialTimeout < 0 || c.IdleTimeout < 0 {
		return fmt.Errorf("%w: 上限に負の値がある", ErrConfig)
	}
	if c.MaxConns == 0 {
		c.MaxConns = defaultMaxConns
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = defaultMaxHeaderBytes
	}
	if c.HeaderTimeout == 0 {
		c.HeaderTimeout = defaultHeaderTimeout
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	s.resolver = c.Resolver
	if s.resolver == nil {
		s.resolver = net.DefaultResolver
	}
	s.allow = make(map[string]struct{}, len(c.Allow))
	for _, a := range c.Allow {
		host, port, reason := splitTarget(a)
		if reason != "" {
			return fmt.Errorf("%w: Allow の %q は host:port (名前と 1〜65535 のポート) ではない (%s)", ErrConfig, a, reason)
		}
		s.allow[hostPort(host, port)] = struct{}{}
	}
	return nil
}

// Serve は、l で接続を受けて、CONNECT を処理する。Close されると ErrClosed を返す。
// l は、返るときに閉じる。Close が、進行中の接続をすべて閉じて、終わるのを待つ。
func (s *Server) Serve(l net.Listener) error {
	defer l.Close()
	if s.err != nil {
		return s.err
	}
	if !s.addListener(l) {
		return ErrClosed
	}
	defer s.removeListener(l)

	var backoff time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return ErrClosed
			}
			var t interface{ Temporary() bool }
			if errors.As(err, &t) && t.Temporary() {
				backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
				select {
				case <-time.After(backoff):
					continue
				case <-s.ctx.Done():
					return ErrClosed
				}
			}
			return err
		}
		backoff = 0
		s.accepted(c)
	}
}

// accepted は、受けた接続を、上限の内なら handle に渡し、外なら 503 で断る。
func (s *Server) accepted(c net.Conn) {
	id := s.nextID.Add(1)
	select {
	case s.sem <- struct{}{}:
	default:
		s.refuse(c, id, deny(503, reasonBusy, ""))
		return
	}
	if !s.addConn(c) {
		<-s.sem
		c.Close()
		return
	}
	go func() {
		defer s.wg.Done()
		defer func() { <-s.sem }()
		defer s.removeConn(c)
		s.handle(c, id)
	}()
}

// handle は、1 本の接続の CONNECT を、読み・判定し・dial して、中継する。
func (s *Server) handle(c net.Conn, id uint64) {
	defer c.Close()
	br := bufio.NewReaderSize(c, readBufSize)

	host, port, rj := s.readRequest(c, br)
	if rj != nil {
		s.refuse(c, id, rj)
		return
	}
	target := hostPort(host, port)
	if _, ok := s.allow[target]; !ok {
		s.refuse(c, id, deny(403, reasonNotAllowed, target))
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.DialTimeout)
	up, ip, err := s.dialUpstream(ctx, host, port)
	cancel()
	if err != nil {
		if s.ctx.Err() != nil {
			return
		}
		rj := classifyDial(err)
		rj.target = target
		s.refuse(c, id, rj)
		return
	}
	defer up.Close()

	// 監査に残せない許可は出さない。
	rec := auditRecord{Event: "allow", Conn: id, Target: target, Reason: reasonAllowlist, Status: 200, IP: ip.String()}
	if err := s.audit(rec); err != nil {
		writeStatus(c, 503)
		return
	}
	if writeStatus(c, 200) != nil {
		return
	}
	relay(s.ctx, c, br, up, s.cfg.IdleTimeout)
}

// dialUpstream は、host を解決して、IP を順に試す。接続する IP の検査は、dial の Control だけが行う。
// 禁止された IP は飛ばし、ほかの IP に接続できればそれを使う。すべて失敗したときは、禁止以外の error を優先して返す。
func (s *Server) dialUpstream(ctx context.Context, host string, port uint16) (net.Conn, netip.Addr, error) {
	addrs, err := s.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, netip.Addr{}, fmt.Errorf("%w: %w", errResolve, err)
	}
	if len(addrs) == 0 {
		return nil, netip.Addr{}, fmt.Errorf("%w: 解決結果が空", errResolve)
	}
	addrs = addrs[:min(len(addrs), maxAddrs)]

	deadline, _ := ctx.Deadline()
	var forbiddenErr, otherErr error
	for i, a := range addrs {
		per := time.Until(deadline) / time.Duration(len(addrs)-i)
		if per <= 0 {
			otherErr = context.DeadlineExceeded
			break
		}
		actx, cancel := context.WithTimeout(ctx, per)
		conn, err := s.dial(actx, "tcp", netip.AddrPortFrom(a, port).String())
		cancel()
		if err == nil {
			return conn, a.Unmap(), nil
		}
		var fe *forbiddenAddrError
		if errors.As(err, &fe) {
			if forbiddenErr == nil {
				forbiddenErr = err
			}
			continue
		}
		if otherErr == nil {
			otherErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if otherErr != nil {
		return nil, netip.Addr{}, otherErr
	}
	return nil, netip.Addr{}, forbiddenErr
}

// Close は、待ち受けと進行中の接続をすべて閉じ、handle が終わるのを待つ。何度呼んでもよい。
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
	cs := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()

	s.cancel()
	var err error
	for _, l := range ls {
		if e := l.Close(); e != nil && !errors.Is(e, net.ErrClosed) && err == nil {
			err = e
		}
	}
	for _, c := range cs {
		c.Close()
	}
	s.wg.Wait()
	return err
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

// addConn は、c を追跡し、wg に数える。Close の後は false を返す (wg.Add を Close の Wait と競合させないため、mu の中で行う)。
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

// hostPort は、許可の表と監査で使う、正規化した "host:port"。
func hostPort(host string, port uint16) string {
	return host + ":" + strconv.Itoa(int(port))
}

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	iwebauthn "github.com/nananek/goronation/cmd/internal/webauthn"
)

// serveUsage は、goro serve -h の使い方。serve (常駐の HTTP サーバー) と token (ブートストラップトークンの
// 発行) の 2 つで、動く場所が違う (token は、サーバーを起こさず、状態ファイルに書くだけ)。
const serveUsage = `使い方: goro serve --listen ADDR --rp-id HOST --origin URL [--state-dir DIR]
        goro serve token [--ttl DURATION] [--state-dir DIR]

serve: WebAuthn (passkey) でログインしたブラウザだけがアクセスできる、最小限の HTTP サーバーを起こす
       (登録・ログインの骨組みだけ。端末ビュー・チャット UI は、まだ無い)。TLS 終端は、tailscale serve の
       ようなリバースプロキシに任せる前提で、goro serve 自身は既定で loopback にしか listen しない。

  --listen ADDR   待ち受ける loopback の TCP (IP リテラル:ポート。127.0.0.1:8443 など)
  --rp-id HOST    WebAuthn の RP ID (--origin のホスト名と、完全に一致すること)
  --origin URL    ブラウザから見た origin ("https://host[:port]"。開発用に http://localhost も可)
  --state-dir DIR 状態を置く場所 (既定は $XDG_STATE_HOME/goro か ~/.local/state/goro)

token: 最初の 1 回だけの登録に使う、ブートストラップトークンを発行し、標準出力に書く。シェルにアクセスできる
       人だけが呼べる想定 (Issue #1 の「初回ログインの信頼は、シェルで発行するアクセストークン」)。すでに
       passkey を登録済みでも発行できるが、登録は最初の 1 回しかできないので、使い道が無い。

  --ttl DURATION  トークンの有効期限 (既定 15m。time.ParseDuration の形)
  --state-dir DIR 状態を置く場所 (serve と同じ既定)
`

// defaultBootstrapTTL は、goro serve token の --ttl の既定値。
const defaultBootstrapTTL = 15 * time.Minute

// sessionCookieName は、ログイン後のセッション token を運ぶ cookie の名前。
const sessionCookieName = "goro_session"

// maxRequestBody は、goro serve が読む要求本体の上限 (WebAuthn の応答は、せいぜい数百バイト〜数 KiB)。
const maxRequestBody = 64 << 10

// serveReadTimeout・serveWriteTimeout・serveIdleTimeout は、*http.Server の全体の締め切り。
// goro serve が読む要求は WebAuthn の応答 (せいぜい数 KiB) だけなので、単純な固定値で十分と判断した
// (egress のような、大きい本文向けの stall 検知・絶対締め切りの仕組みは要らない)。ReadHeaderTimeout
// (既存) はヘッダだけに効き、本文の読み取りには効かないので、ReadTimeout も別に設定する。
const (
	serveReadTimeout  = 30 * time.Second
	serveWriteTimeout = 30 * time.Second
	serveIdleTimeout  = 60 * time.Second
)

// maxServeConns は、goro serve が同時に受理する接続数の上限 (limitedListener が絞る)。1 ユーザー・
// 少数のブラウザ/タブを想定した個人用ツールなので、余裕を持たせつつ、無制限にはしない。
const maxServeConns = 64

// runServe は goro serve の本体。serve と token へ振り分ける。
func runServe(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "token" {
		return runServeToken(args[1:], stdout, stderr)
	}
	return runServeServer(args, stderr)
}

// serveFlags は、serve と token に共通する --state-dir と、getopt 的な解析の下ごしらえ。
func serveFlags(name string, args []string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, serveUsage) }
	return flags, flags.String("state-dir", "", "")
}

func runServeServer(args []string, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro serve: "+format+"\n", a...)
		return exitUsage
	}
	flags, stateDirFlag := serveFlags("goro serve", args, stderr)
	listen := flags.String("listen", "", "")
	rpID := flags.String("rp-id", "", "")
	origin := flags.String("origin", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		return fail("余計な引数 %q", flags.Arg(0))
	}
	if *listen == "" || *rpID == "" || *origin == "" {
		return fail("--listen・--rp-id・--origin は、どれも要る")
	}
	ap, err := netip.ParseAddrPort(*listen)
	if err != nil || !ap.Addr().IsLoopback() {
		return fail("--listen は loopback の IP リテラル:ポートだけ (127.0.0.1:8443 など): %q", *listen)
	}
	cfg := iwebauthn.Config{RPID: *rpID, RPName: "goro", Origin: *origin}
	if err := cfg.Validate(); err != nil {
		return fail("%v", err)
	}
	stateDir, err := resolveStateDir(*stateDirFlag)
	if err != nil {
		return fail("%v", err)
	}
	store, err := iwebauthn.NewStore(stateDir)
	if err != nil {
		return fail("%v", err)
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail("待ち受けられない: %v", err)
	}
	defer l.Close()
	srv := newServeHTTPServer(newServeMux(cfg, store, *origin))
	fmt.Fprintf(stderr, "goro serve: %s で待ち受けている (rp-id=%s origin=%s)\n", l.Addr(), sanitize(*rpID), sanitize(*origin))
	if err := srv.Serve(newLimitedListener(l, maxServeConns)); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "goro serve: 終了: %v\n", err)
		return 1
	}
	return 0
}

// limitedListener は、net.Listener を薄くラップし、同時に受理する接続数を max で絞る (semaphore)。
// 空きが無い間は Accept が待つ (accept せずに待たせるので、キューにも積まない)。Close は、待っている
// Accept も含めて、すぐに終わらせる (http.Server.Close は Listener.Close を呼ぶので、そのまま繋がる)。
type limitedListener struct {
	net.Listener
	sem  chan struct{}
	done chan struct{}
	once sync.Once
}

func newLimitedListener(l net.Listener, max int) *limitedListener {
	return &limitedListener{Listener: l, sem: make(chan struct{}, max), done: make(chan struct{})}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitedConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitedConn は、Close されたときに、limitedListener の枠を 1 つ空ける (二重に空けない)。
type limitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *limitedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// newServeHTTPServer は、goro serve が実際に使う *http.Server を組み立てる (runServeServer と、結合
// テスト (実際に生の TCP を張って確かめるもの) の、両方がこれを呼ぶ。同じ設定を、別々に書いて食い違わせない
// ため)。goro serve が読む要求は WebAuthn の応答 (せいぜい数 KiB) だけなので、単純な固定値の締め切りで
// 十分と判断した (serveReadTimeout 等の doc comment を参照)。
//
// keep-alive は無効にする: limitedListener の枠は接続が閉じるまで解放されないが、keep-alive 中の接続は
// 応答後も閉じず、IdleTimeout (無通信の締め切り) でしか切れない。1 リクエストごとに枠を確実に返すには、
// 1 接続=最大 1 リクエストに強制する必要がある (攻撃者視点レビューで実証された、正当なリクエストを
// IdleTimeout の内側で周期的に送るだけで枠を無期限に占有できる、という可用性 DoS への対応)。これは
// 「その接続への再利用」を閉じるだけで、body をゆっくり送りながら都度接続を張り直す変種までは防がない
// (doc.go の「限界」を参照)。
func newServeHTTPServer(handler http.Handler) *http.Server {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       serveReadTimeout,
		WriteTimeout:      serveWriteTimeout,
		IdleTimeout:       serveIdleTimeout,
	}
	srv.SetKeepAlivesEnabled(false)
	return srv
}

func runServeToken(args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro serve token: "+format+"\n", a...)
		return exitUsage
	}
	flags, stateDirFlag := serveFlags("goro serve token", args, stderr)
	ttl := flags.Duration("ttl", defaultBootstrapTTL, "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		return fail("余計な引数 %q", flags.Arg(0))
	}
	if *ttl <= 0 {
		return fail("--ttl は正の期間で書く: %q", ttl.String())
	}
	stateDir, err := resolveStateDir(*stateDirFlag)
	if err != nil {
		return fail("%v", err)
	}
	store, err := iwebauthn.NewStore(stateDir)
	if err != nil {
		return fail("%v", err)
	}
	tok, err := iwebauthn.IssueBootstrapToken(context.Background(), store, *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "goro serve token: 発行できない: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	fmt.Fprintf(stderr, "goro serve token: %s の間、有効。goro serve の画面で、登録のときにこれを貼る\n", ttl.String())
	return 0
}

// newServeMux は、goro serve の HTTP のハンドラをまとめる。
func newServeMux(cfg iwebauthn.Config, store *iwebauthn.Store, origin string) http.Handler {
	s := &server{cfg: cfg, store: store, secure: hasHTTPSScheme(origin)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /static/app.js", s.handleAppJS)
	mux.HandleFunc("POST /webauthn/register/begin", s.handleRegisterBegin)
	mux.HandleFunc("POST /webauthn/register/finish", s.handleRegisterFinish)
	mux.HandleFunc("POST /webauthn/login/begin", s.handleLoginBegin)
	mux.HandleFunc("POST /webauthn/login/finish", s.handleLoginFinish)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /api/whoami", s.requireSession(s.handleWhoami))
	return securityHeaders(mux)
}

// server は、ハンドラが共有する、リクエストをまたいで変わらない値。
type server struct {
	cfg    iwebauthn.Config
	store  *iwebauthn.Store
	secure bool // origin が https か (cookie の Secure 属性に使う。開発用の http://localhost では false)
}

func hasHTTPSScheme(origin string) bool { return len(origin) >= 8 && origin[:8] == "https://" }

// securityHeaders は、既定で全ての応答に付ける、最小限のセキュリティヘッダ。インライン script は許さない
// (フロントエンドの JS は /static/app.js から読む)。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

func (s *server) handleAppJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	io.WriteString(w, appJS)
}

// readJSON は、r の本体を上限つきで読み、v へ JSON として読む。
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	return json.NewDecoder(r.Body).Decode(v)
}

// writeJSON は、v を JSON で書く。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError は、status と、外へ出してよい短い label を返す。詳しい理由 (err) は、呼び手が別途ログに出す。
func writeError(w http.ResponseWriter, status int, label string) {
	writeJSON(w, status, map[string]string{"error": label})
}

type registerBeginRequest struct {
	Token string `json:"token"`
}
type registerFinishRequest struct {
	State      string                        `json:"state"`
	Credential iwebauthn.AttestationResponse `json:"credential"`
}
type loginFinishRequest struct {
	State      string                      `json:"state"`
	Credential iwebauthn.AssertionResponse `json:"credential"`
}

func (s *server) handleRegisterBegin(w http.ResponseWriter, r *http.Request) {
	var req registerBeginRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "壊れた要求")
		return
	}
	opts, state, err := iwebauthn.RegisterBegin(r.Context(), s.cfg, s.store, req.Token)
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, iwebauthn.ErrAlreadyRegistered) {
			status = http.StatusConflict
		}
		writeError(w, status, "登録を始められない")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": opts, "state": state})
}

func (s *server) handleRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var req registerFinishRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "壊れた要求")
		return
	}
	if err := iwebauthn.RegisterFinish(r.Context(), s.cfg, s.store, req.State, req.Credential); err != nil {
		writeError(w, http.StatusForbidden, "登録を確認できない")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	opts, state, err := iwebauthn.AuthenticateBegin(r.Context(), s.cfg, s.store)
	if err != nil {
		writeError(w, http.StatusNotFound, "ログインできない")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": opts, "state": state})
}

func (s *server) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	var req loginFinishRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "壊れた要求")
		return
	}
	session, err := iwebauthn.AuthenticateFinish(r.Context(), s.cfg, s.store, req.State, req.Credential)
	if err != nil {
		// ログイン失敗の詳しい理由は、応答に出さない (総当たりの手がかりにしない)。
		writeError(w, http.StatusUnauthorized, "ログインできない")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: session, Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(iwebauthn.SessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout は、cookie を消すだけでなく、サーバー側でも世代番号を進めて、発行済みの全セッション
// token (このブラウザ以外が持っているものも含む) を失効させる。盗まれた cookie が logout 後も有効期限
// いっぱい通ってしまう、という攻撃者視点レビューの指摘への対応。
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := iwebauthn.Logout(r.Context(), s.store); err != nil {
		writeError(w, http.StatusInternalServerError, "ログアウトできない")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requireSession は、next を、有効なセッション cookie があるときだけ呼ぶ。無ければ 401。
func (s *server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "ログインが要る")
			return
		}
		if err := iwebauthn.VerifySession(r.Context(), s.store, c.Value); err != nil {
			writeError(w, http.StatusUnauthorized, "ログインが要る")
			return
		}
		next(w, r)
	}
}

func (s *server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

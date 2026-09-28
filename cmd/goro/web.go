//go:build linux

package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nananek/goronation/cmd/internal/session"
	iwebauthn "github.com/nananek/goronation/cmd/internal/webauthn"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// webUsage は、goro web -h の使い方。web (常駐の HTTP サーバー) と token (ブートストラップトークンの
// 発行) の 2 つで、動く場所が違う (token は、サーバーを起こさず、状態ファイルに書くだけ)。
const webUsage = `使い方: goro web --listen ADDR --rp-id HOST --origin URL [--state-dir DIR] [--repos-dir DIR]
        goro web token [--ttl DURATION] [--state-dir DIR]

web: WebAuthn (passkey) でログインしたブラウザだけがアクセスできる、常駐の HTTP サーバーを起こす。
     登録・ログイン・セッション一覧・repo のファイルブラウザ・端末ビュー (xterm.js) の画面を、全部
     ここが持つ。実際のエージェント (檻) は goro serve (UDS 専用) が起動・保持し、goro web は認証済みの
     WebSocket 接続を、対応する goro serve の UDS へ、中身を解釈しない素通しで中継する (必要なら
     goro serve を子プロセスとして起動する)。TLS 終端は、tailscale serve のようなリバースプロキシに
     任せる前提で、goro web 自身は既定で loopback にしか listen しない。

  --listen ADDR    待ち受ける loopback の TCP (IP リテラル:ポート。127.0.0.1:8443 など)
  --rp-id HOST     WebAuthn の RP ID (--origin のホスト名と、完全に一致すること)
  --origin URL     ブラウザから見た origin ("https://host[:port]"。開発用に http://localhost も可)
  --state-dir DIR  状態を置く場所 (既定は $XDG_STATE_HOME/goro か ~/.local/state/goro)
  --repos-dir DIR  新しいセッションを始められる repo を置く、1 つのディレクトリ (直下の git repo を
                   列挙する。指定しなければ、ファイルブラウザ機能自体が使えない: 既存のセッションの
                   一覧・再開だけができる。複数の置き場所は設定できない)

token: 最初の 1 回だけの登録に使う、ブートストラップトークンを発行し、標準出力に書く。シェルにアクセスできる
       人だけが呼べる想定 (Issue #1 の「初回ログインの信頼は、シェルで発行するアクセストークン」)。すでに
       passkey を登録済みでも発行できるが、登録は最初の 1 回しかできないので、使い道が無い。

  --ttl DURATION  トークンの有効期限 (既定 15m。time.ParseDuration の形)
  --state-dir DIR 状態を置く場所 (web と同じ既定)
`

// defaultBootstrapTTL は、goro web token の --ttl の既定値。
const defaultBootstrapTTL = 15 * time.Minute

// sessionCookieName は、ログイン後のセッション token を運ぶ cookie の名前。
const sessionCookieName = "goro_session"

// maxRequestBody は、goro web が読む要求本体の上限 (WebAuthn の応答・API の要求は、せいぜい数 KiB)。
const maxRequestBody = 64 << 10

// webReadTimeout・webWriteTimeout・webIdleTimeout は、*http.Server の全体の締め切り (egress のような、
// 大きい本文向けの stall 検知・絶対締め切りの仕組みは要らない)。keep-alive は無効にする: limitedListener
// の枠は接続が閉じるまで解放されないが、keep-alive 中の接続は応答後も閉じず、IdleTimeout (無通信の
// 締め切り) でしか切れない (攻撃者視点レビューで実証された、可用性 DoS への対応。PR #35・#36 の教訓を
// そのまま引き継ぐ)。
const (
	webReadTimeout  = 30 * time.Second
	webWriteTimeout = 30 * time.Second
	webIdleTimeout  = 60 * time.Second
)

// maxWebConns は、goro web が同時に受理する接続数の上限 (limitedListener が絞る)。1 ユーザー・少数の
// ブラウザ/タブを想定した個人用ツールなので、余裕を持たせつつ、無制限にはしない。
const maxWebConns = 64

// webShutdownTimeout は、SIGTERM 等を受けてからの graceful shutdown (http.Server.Shutdown) に許す猶予。
const webShutdownTimeout = 5 * time.Second

// runWeb は goro web の本体。web と token へ振り分ける。
func runWeb(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "token" {
		return runWebToken(args[1:], stdout, stderr)
	}
	return runWebServer(args, stderr)
}

// webFlags は、web と token に共通する --state-dir と、getopt 的な解析の下ごしらえ。
func webFlags(name string, args []string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, webUsage) }
	return flags, flags.String("state-dir", "", "")
}

func runWebServer(args []string, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro web: "+format+"\n", a...)
		return exitUsage
	}
	flags, stateDirFlag := webFlags("goro web", args, stderr)
	listen := flags.String("listen", "", "")
	rpID := flags.String("rp-id", "", "")
	origin := flags.String("origin", "", "")
	reposDirFlag := flags.String("repos-dir", "", "")
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
	reposDir := *reposDirFlag
	if reposDir != "" {
		abs, err := resolveReposDir(reposDir)
		if err != nil {
			return fail("--repos-dir: %v", err)
		}
		reposDir = abs
	}
	store, err := iwebauthn.NewStore(stateDir)
	if err != nil {
		return fail("%v", err)
	}
	self, err := os.Executable()
	if err == nil {
		self, err = resolveExe(self)
	}
	if err != nil {
		return fail("goro 自身の実行ファイルを決められない: %v", err)
	}
	sessStore, err := session.NewStore(stateDir, bwrap.CurrentHost())
	if err != nil {
		return fail("%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSig := watchServeSignals(cancel)
	defer stopSig()

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail("待ち受けられない: %v", err)
	}
	defer l.Close()
	srv := newWebHTTPServer(newWebMux(cfg, store, *origin, sessStore, reposDir, stateDir, self))
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), webShutdownTimeout)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(stderr, "goro web: %s で待ち受けている (rp-id=%s origin=%s)\n", l.Addr(), sanitize(*rpID), sanitize(*origin))
	if err := srv.Serve(newLimitedListener(l, maxWebConns)); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "goro web: 終了: %v\n", err)
		return 1
	}
	return 0
}

// resolveReposDir は、--repos-dir を、絶対・実在するディレクトリの path にする。
func resolveReposDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s がディレクトリではない", abs)
	}
	return abs, nil
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

// newWebHTTPServer は、goro web が実際に使う *http.Server を組み立てる (runWebServer と、結合テスト
// (実際に生の TCP を張って確かめるもの) の、両方がこれを呼ぶ。同じ設定を、別々に書いて食い違わせない
// ため)。
func newWebHTTPServer(handler http.Handler) *http.Server {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       webReadTimeout,
		WriteTimeout:      webWriteTimeout,
		IdleTimeout:       webIdleTimeout,
	}
	srv.SetKeepAlivesEnabled(false)
	return srv
}

func runWebToken(args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "goro web token: "+format+"\n", a...)
		return exitUsage
	}
	flags, stateDirFlag := webFlags("goro web token", args, stderr)
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
		fmt.Fprintf(stderr, "goro web token: 発行できない: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	fmt.Fprintf(stderr, "goro web token: %s の間、有効。goro web の画面で、登録のときにこれを貼る\n", ttl.String())
	return 0
}

// newWebMux は、goro web の HTTP のハンドラをまとめる。
func newWebMux(cfg iwebauthn.Config, store *iwebauthn.Store, origin string, sessStore *session.Store, reposDir, stateDir, self string) http.Handler {
	s := &webServer{cfg: cfg, store: store, secure: hasHTTPSScheme(origin), sessions: sessStore, reposDir: reposDir, stateDir: stateDir, self: self}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /static/app.js", s.handleAppJS)
	mux.HandleFunc("GET /static/sessions.js", s.handleSessionsJS)
	mux.HandleFunc("GET /static/terminal.js", s.handleTerminalJS)
	mux.HandleFunc("GET /static/vendor/xterm.js", serveEmbedded(xtermVendor, "vendor/xterm/xterm.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /static/vendor/xterm.css", serveEmbedded(xtermVendor, "vendor/xterm/xterm.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /static/vendor/addon-fit.js", serveEmbedded(xtermVendor, "vendor/xterm/addon-fit.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("POST /webauthn/register/begin", s.handleRegisterBegin)
	mux.HandleFunc("POST /webauthn/register/finish", s.handleRegisterFinish)
	mux.HandleFunc("POST /webauthn/login/begin", s.handleLoginBegin)
	mux.HandleFunc("POST /webauthn/login/finish", s.handleLoginFinish)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /api/whoami", s.requireSession(s.handleWhoami))
	mux.HandleFunc("GET /sessions", s.requireSession(s.handleSessionsPage))
	mux.HandleFunc("GET /api/sessions", s.requireSession(s.handleSessionsList))
	mux.HandleFunc("GET /api/repos", s.requireSession(handleReposList(reposDir)))
	mux.HandleFunc("POST /api/repos/start", s.requireSession(s.handleRepoStart))
	mux.HandleFunc("GET /s/{id}", s.requireSession(s.handleTerminalPage))
	mux.HandleFunc("GET /s/{id}/ws", s.requireSession(s.handleTerminalProxy))
	return securityHeaders(mux)
}

// webServer は、ハンドラが共有する、リクエストをまたいで変わらない値。
type webServer struct {
	cfg      iwebauthn.Config
	store    *iwebauthn.Store
	secure   bool // origin が https か (cookie の Secure 属性に使う。開発用の http://localhost では false)
	sessions *session.Store
	reposDir string // --repos-dir (絶対 path。空なら未設定)
	stateDir string
	self     string // goro 自身の実行ファイル (symlink を辿った実体。goro serve を起こすときに使う)
}

func hasHTTPSScheme(origin string) bool { return len(origin) >= 8 && origin[:8] == "https://" }

// securityHeaders は、既定で全ての応答に付ける、最小限のセキュリティヘッダ。インライン script は許さない
// (フロントエンドの JS は /static/*.js から読む)。
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

func (s *webServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

func (s *webServer) handleAppJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	io.WriteString(w, appJS)
}

func (s *webServer) handleSessionsJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	io.WriteString(w, sessionsJS)
}

func (s *webServer) handleTerminalJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	io.WriteString(w, terminalJS)
}

// serveEmbedded は、fsys の name (vendor から埋め込んだファイル) を、contentType で返すハンドラを作る。
func serveEmbedded(fsys embed.FS, name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fsys.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(b)
	}
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

func (s *webServer) handleRegisterBegin(w http.ResponseWriter, r *http.Request) {
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

func (s *webServer) handleRegisterFinish(w http.ResponseWriter, r *http.Request) {
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

func (s *webServer) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	opts, state, err := iwebauthn.AuthenticateBegin(r.Context(), s.cfg, s.store)
	if err != nil {
		writeError(w, http.StatusNotFound, "ログインできない")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": opts, "state": state})
}

func (s *webServer) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	var req loginFinishRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "壊れた要求")
		return
	}
	sess, err := iwebauthn.AuthenticateFinish(r.Context(), s.cfg, s.store, req.State, req.Credential)
	if err != nil {
		// ログイン失敗の詳しい理由は、応答に出さない (総当たりの手がかりにしない)。
		writeError(w, http.StatusUnauthorized, "ログインできない")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: sess, Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(iwebauthn.SessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout は、cookie を消すだけでなく、呼び手が有効なセッション cookie を提示できたときだけ、
// サーバー側でも世代番号を進めて、発行済みの全セッション token (このブラウザ以外が持っているものも
// 含む) を失効させる (PR #35 の攻撃者視点レビューの指摘への対応。詳細はそちらの記録を参照)。
func (s *webServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if err := iwebauthn.VerifySession(r.Context(), s.store, c.Value); err == nil {
			if err := iwebauthn.Logout(r.Context(), s.store); err != nil {
				writeError(w, http.StatusInternalServerError, "ログアウトできない")
				return
			}
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requireSession は、next を、有効なセッション cookie があるときだけ呼ぶ。無ければ 401。
func (s *webServer) requireSession(next http.HandlerFunc) http.HandlerFunc {
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

func (s *webServer) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

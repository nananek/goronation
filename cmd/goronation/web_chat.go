//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// chatRelayConfig は、goronation web が、goronation serve --chat (UDS) へ中継するときの、上限と期限 (ADR 0014)。serve の http.Server には
// タイムアウトが無く、SSE の心拍も無い (ADR 0013 の Limit)。接続の枠・期限・心拍は、web が持つ。テストが短くできるよう、値は構造体にする。
type chatRelayConfig struct {
	MaxSSE           int           // 全体の SSE の同時接続の上限 (web の接続枠 maxWebConns より小さく、serve の 32 より小さい)
	MaxSSEPerSession int           // セッション 1 つあたりの SSE の上限 (serve の 32 を、1 つのセッションが使い切らない)
	DialTimeout      time.Duration // UDS への接続
	HeaderTimeout    time.Duration // 要求を送ってから、応答のヘッダが来るまで (止まった serve が、枠を握らない)
	CallTimeout      time.Duration // 書き込み API の、要求から応答の最後まで
	SSEWriteTimeout  time.Duration // SSE の 1 回の書き込み (読まない購読者が、web の枠も serve の枠も握り続けない)
	Heartbeat        time.Duration // 無通信のとき、`: ping` を送る間隔
}

var defaultChatRelay = chatRelayConfig{
	MaxSSE: 16, MaxSSEPerSession: 4,
	DialTimeout: 2 * time.Second, HeaderTimeout: 5 * time.Second, CallTimeout: 10 * time.Second,
	SSEWriteTimeout: 10 * time.Second, Heartbeat: 15 * time.Second,
}

// chatMaxEvent は、SSE の 1 イベント (data の 1 行) の大きさの上限。serve の Event の上限 (chat.DefaultMaxEvent) より余裕を持たせる。
const chatMaxEvent = 4 << 20

// chatRelay は、SSE の枠の勘定。
type chatRelay struct {
	cfg chatRelayConfig

	mu         sync.Mutex
	total      int
	perSession map[string]int
}

// acquire は、id の SSE の枠を 1 つ取る (全体・セッション単位のどちらの上限も超えないときだけ)。返す関数で戻す。
func (c *chatRelay) acquire(id string) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total >= c.cfg.MaxSSE || c.perSession[id] >= c.cfg.MaxSSEPerSession {
		return nil, false
	}
	if c.perSession == nil {
		c.perSession = map[string]int{}
	}
	c.total++
	c.perSession[id]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.total--
			if c.perSession[id]--; c.perSession[id] <= 0 {
				delete(c.perSession, id)
			}
		})
	}, true
}

// chatClient は、id の chat.sock だけへ繋がる http.Client (URL の host は無視する)。keep-alive なし・応答のヘッダに期限つき。本体の長さには期限を
// 付けない (SSE)。呼び手が ctx で切る。goronation serve を、起こさない (chat.sock が無ければ、繋がらないだけ)。
func (s *webServer) chatClient(id string) *http.Client {
	sock := chatSocketPath(s.stateDir, defaultGroup, id)
	dialer := net.Dialer{Timeout: s.chat.cfg.DialTimeout}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           func(ctx context.Context, _, _ string) (net.Conn, error) { return dialer.DialContext(ctx, "unix", sock) },
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: s.chat.cfg.HeaderTimeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// chatUpstreamError は、serve への呼び出しの失敗を、status にして返す (呼び手が、書けるならクライアントに返す)。
func chatUpstreamError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
		// クライアントが切った: 返す相手がいない
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		writeError(w, http.StatusNotFound, "チャットが起動していない")
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		writeError(w, http.StatusGatewayTimeout, "チャットが応答しない")
	default:
		writeError(w, http.StatusBadGateway, "チャットに繋がらない")
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// chatSessionID は、URL の {id} を確かめる (session.Store の ID の形だけ。path traversal を断つ)。
func chatSessionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !termIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "壊れたセッション ID")
		return "", false
	}
	return id, true
}

// handleChatEvents は、requireSession で保護された GET /s/{id}/events。serve の GET /events (SSE) を、自前の行コピーで中継する:
// 1 イベント (data の行と空行) ずつ、書き込みの期限を付けて書き、Flush する。無通信なら心拍 (`: ping`) を書く。クライアントが切れれば、serve への接続も閉じる。
// serve の心拍は無いので、web が持つ。HEAD は、serve へ流さない (serve は HEAD でも SSE の枠を使う)。
func (s *webServer) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "GET だけ")
		return
	}
	id, ok := chatSessionID(w, r)
	if !ok {
		return
	}
	release, ok := s.chat.acquire(id)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "SSE の接続が多すぎる")
		return
	}
	defer release()

	// http.Server の WriteTimeout (30 秒) は、接続の書きの締め切りとして残る。書くたびに、この接続の締め切りを延ばす (SSE が、30 秒で切れない)。
	rc := http.NewResponseController(w)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel() // 戻ったら、serve への接続も閉じる
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://chat.sock/events"+chatEventsQuery(r.URL.Query()), nil)
	resp, err := s.chatClient(id).Do(req)
	if err != nil {
		chatUpstreamError(w, r.Context(), err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		if resp.StatusCode == http.StatusServiceUnavailable {
			writeError(w, http.StatusServiceUnavailable, "チャットの SSE の接続が多すぎる")
		} else {
			writeError(w, http.StatusBadGateway, "チャットの応答が SSE でない")
		}
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	rc.SetWriteDeadline(time.Now().Add(s.chat.cfg.SSEWriteTimeout))
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}

	events := make(chan []byte)
	go func() {
		defer close(events)
		readSSEEvents(ctx, resp.Body, events)
	}()
	write := func(b []byte) bool {
		rc.SetWriteDeadline(time.Now().Add(s.chat.cfg.SSEWriteTimeout))
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	beat := time.NewTimer(s.chat.cfg.Heartbeat)
	defer beat.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !write(ev) {
				return
			}
			if !beat.Stop() {
				select {
				case <-beat.C:
				default:
				}
			}
			beat.Reset(s.chat.cfg.Heartbeat)
		case <-beat.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
			beat.Reset(s.chat.cfg.Heartbeat)
		case <-ctx.Done():
			return
		}
	}
}

var (
	chatAfterRE      = regexp.MustCompile(`^[0-9]{1,16}$`)
	chatGenerationRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	chatIDLineRE     = regexp.MustCompile(`^id: [0-9]{1,16}$`)
)

// chatEventsQuery は、クライアントの GET /events のクエリから、serve に渡すクエリを作る (ADR 0024 決定 1・ADR 0054): after (10 進数 16 桁まで)・generation (小文字の 16 進 32 桁) が、
// どちらも 1 つだけで、形が正しいときだけ、url.Values で作り直して渡す。RawQuery は、そのまま渡さない (余分なキー・重複キー・制御文字は、serve に届かない)。
// 不正・欠けは、クエリなし (= 全再送。エラーにしない)。値の意味 (世代の一致・after の範囲) は、serve が確かめる。
func chatEventsQuery(q url.Values) string {
	a, g := q["after"], q["generation"]
	if len(a) != 1 || len(g) != 1 || !chatAfterRE.MatchString(a[0]) || !chatGenerationRE.MatchString(g[0]) {
		return ""
	}
	return "?" + url.Values{"after": {a[0]}, "generation": {g[0]}}.Encode()
}

// readSSEEvents は、serve の SSE を、1 イベント (空行までの行。末尾の空行を含む) ずつ、out に送る。フレーミングを確かめる: 行は、`event: `・`data: `・
// 空行と、`id: <10 進数>` (16 桁まで。1 イベントに 1 行だけ。ADR 0054)。ほかの行・大きすぎるイベントは、そこで止める (serve は信頼するが、上流のバグが、クライアントへの別のイベントの偽造にならないように)。
func readSSEEvents(ctx context.Context, r io.Reader, out chan<- []byte) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, chatMaxEvent)
	var ev bytes.Buffer
	idSeen := false
	for sc.Scan() {
		line := sc.Bytes()
		if chatIDLineRE.Match(line) {
			if idSeen {
				return
			}
			idSeen = true
		} else if len(line) > 0 && !bytes.HasPrefix(line, []byte("event: ")) && !bytes.HasPrefix(line, []byte("data: ")) {
			return
		}
		if ev.Len()+len(line)+1 > chatMaxEvent {
			return
		}
		ev.Write(line)
		ev.WriteByte('\n')
		if len(line) == 0 {
			idSeen = false
			b := append([]byte(nil), ev.Bytes()...)
			ev.Reset()
			select {
			case out <- b:
			case <-ctx.Done():
				return
			}
		}
	}
}

// checkChatWrite は、書き込み系のルート共通の関門 (CSRF): Origin が --origin と完全に一致すること (無い・null・別は 403)、Sec-Fetch-Site があれば
// same-origin であること、Content-Type が application/json であること (415)。SameSite=Strict の cookie に加える、二重目。requireSession の後で呼ぶ。
func (s *webServer) checkChatWrite(w http.ResponseWriter, r *http.Request) bool {
	if o := r.Header.Get("Origin"); o == "" || o != s.origin {
		writeError(w, http.StatusForbidden, "Origin が違う")
		return false
	}
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" {
		writeError(w, http.StatusForbidden, "同じ origin からだけ")
		return false
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type は application/json")
		return false
	}
	return true
}

// chatWriteRelayStatus は、serve の応答のうち、そのまま返す status (ほかは 502)。
var chatWriteRelayStatus = map[int]bool{200: true, 400: true, 404: true, 409: true, 413: true, 415: true, 500: true}

// handleChatWrite は、POST /s/{id}/<op> (message・permission・form・stop) を、serve の POST /<op> へ中継する。本文は、解析せず、そのまま渡す (サイズの上限と
// Content-Type の検査だけ)。検証は serve が 1 か所で行う: web が別の解析器で検証すると、重複キー・大文字小文字の違うキーで、検証と実行が食い違いうる
// (ADR 0013 の L-G)。世代・request_id・outcome も、変えず・補わず渡す。serve を起こさない (chat.sock が無ければ 404)。
func (s *webServer) handleChatWrite(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkChatWrite(w, r) {
			return
		}
		id, ok := chatSessionID(w, r)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, chatMaxBody))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "本文が大きすぎる")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.chat.cfg.CallTimeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://chat.sock/"+op, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.chatClient(id).Do(req)
		if err != nil {
			chatUpstreamError(w, r.Context(), err)
			return
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			chatUpstreamError(w, ctx, err)
			return
		}
		if !chatWriteRelayStatus[resp.StatusCode] {
			writeError(w, http.StatusBadGateway, "チャットの応答が不正")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(resp.StatusCode)
		w.Write(out)
	}
}

// handleChatPage は、requireSession で保護された GET /s/{id}/chat (構造化チャットの画面。表示だけ)。serve には触れない (起こさない・繋がない):
// 画面が、EventSource で /s/{id}/events を開く。{id} は形だけ確かめる。
func (s *webServer) handleChatPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := chatSessionID(w, r); !ok {
		return
	}
	b, err := chatAssets.ReadFile("assets/chat/chat.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// maxChatStarts は、同時に起動待ちにできる chat の数 (serve の起動は、最大 spawnReadyTimeout かかり、その間、web の接続を使う)。
const maxChatStarts = 4

// chatStartGuard は、POST /api/chat/start の、二重起動の防ぎ: 同じ repo の起動待ちは 1 つだけ (二重クリック・二重送信で、
// 同じ repo の chat が 2 つ起動して、クレジットを 2 重に使わない)。
type chatStartGuard struct {
	mu   sync.Mutex
	busy map[string]bool
}

// acquire は、repo (path) の起動待ちの枠を取る。すでに待っている (同じ repo) なら dup、全体の上限なら full。
func (g *chatStartGuard) acquire(repo string) (release func(), dup, full bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy[repo] {
		return nil, true, false
	}
	if len(g.busy) >= maxChatStarts {
		return nil, false, true
	}
	if g.busy == nil {
		g.busy = map[string]bool{}
	}
	g.busy[repo] = true
	return func() {
		g.mu.Lock()
		delete(g.busy, repo)
		g.mu.Unlock()
	}, false, false
}

// chatSockPlaceholderID は、まだ作っていないセッションの ID の代わり (同じ長さ。UDS の path の長さの検査に使う)。
const chatSockPlaceholderID = "00000000-000000-000000"

// chatSockExists は、id の chat.sock (ソケットのファイル) が、あるか。serve --chat が動いているか、動いていた印 (前の serve の残りは、
// 繋がるまで分からない。会話の画面は、繋がらなければ、そう表示する)。
func chatSockExists(stateDir, id string) bool {
	fi, err := os.Lstat(chatSocketPath(stateDir, defaultGroup, id))
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// handleChatStart は、requireSession で保護された POST /api/chat/start {repo}: --repos-dir 直下の repo (name) を、goronation serve --chat --repo で
// 新しいセッションとして起動し、{id} を返す (最初の指示は、チャットの画面から送る: 起動しただけでは、エージェントに何も送らず、クレジットを使わない)。
// 書き込みの関門 (Origin・Sec-Fetch-Site・Content-Type) を通す。同じ repo の起動待ちが、すでにあれば 409 (二重起動しない)。
// 端末ビューの POST /api/repos/start は、変えない。
func (s *webServer) handleChatStart(w http.ResponseWriter, r *http.Request) {
	if !s.checkChatWrite(w, r) {
		return
	}
	var req repoStartRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "壊れた要求")
		return
	}
	repoPath, err := resolveRepoName(s.reposDir, req.Repo)
	if err != nil {
		var he httpError
		if errors.As(err, &he) {
			writeError(w, he.status, he.label)
			return
		}
		writeError(w, http.StatusBadRequest, "repo を解決できない")
		return
	}
	// serve は、UDS の path が長いと、セッションを作ってから失敗する (L-D)。作る前に、断る。
	if len(chatSocketPath(s.stateDir, defaultGroup, chatSockPlaceholderID)) > maxSockPath {
		writeError(w, http.StatusInternalServerError, "state-dir が長すぎて、chat.sock を作れない (--state-dir を短くする)")
		return
	}
	release, dup, full := s.chatStarts.acquire(repoPath)
	if dup {
		writeError(w, http.StatusConflict, "この repo のチャットを起動中 (少し待つ)")
		return
	}
	if full {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "起動待ちのチャットが多すぎる")
		return
	}
	defer release()
	id, err := spawnServe(s.stateDir, s.self, []string{"--chat", "--repo", repoPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "goronation web: チャットを起動できない: %s\n", sanitize(err.Error()))
		if strings.Contains(err.Error(), "root では動かせない") {
			writeError(w, http.StatusInternalServerError, "チャットは root では動かせない (goronation web を、非 root の利用者で動かす)")
			return
		}
		writeError(w, http.StatusBadGateway, "チャットを起動できない")
		return
	}
	writeJSON(w, http.StatusOK, repoStartResponse{ID: id})
}

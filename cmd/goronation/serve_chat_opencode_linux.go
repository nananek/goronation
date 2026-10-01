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
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// opencode の chat (HTTP の transport。ADR 0051): init の状態の 1 行 → SSE (GET /api/event) を先に開く → session の作成 → root の一致の確認 →
// SSE の data: を会話へ。会話の指示は httpWriter が、独立に検査して、HTTP で送る。HTTP は、control (init の標準入力の socketpair) に fd を
// 送る relayClient 越しだけで、このホストに待ち受けは増えない。

// transport の待ち時間 (試験で縮めるので var)。
var (
	opencodeReadyTimeout   = 60 * time.Second // init の版の検査 (10 秒)・起動の証明 (30 秒)・余裕
	opencodeConnectTimeout = 10 * time.Second // SSE の server.connected・session の作成の応答・root の session.created
	opencodeRequestTimeout = 30 * time.Second // 会話の指示の HTTP 1 回
)

// opencodeReadyMax は、init の状態の 1 行の上限 (バイト)。opencodeBodyMax は、応答の本文を読む上限 (バイト)。
const (
	opencodeReadyMax = 4 << 10
	opencodeBodyMax  = 1 << 20
)

// opencodeSessionBody は、POST /api/session の本文: permissions は、順序が本質 (あとの規則が勝つ。ADR 0043 決定 2): すべてを ask にし、
// question だけ allow にする (承認の段を省き、form が直接出る)。順序を逆にすると、あとの * が勝って、question も承認を要求する。
const opencodeSessionBody = `{"permissions":[{"action":"*","resource":"*","effect":"ask"},{"action":"question","resource":"*","effect":"allow"}]}`

// errOpencodeInit は、init が {"error":…} を返した (起動の失敗。ポートが使われていた場合を含む)。
type errOpencodeInit struct{ reason string }

func (e *errOpencodeInit) Error() string { return "opencode を起動できない: " + e.reason }

// readReadyLine は、init の状態の 1 行 (標準出力) を読む: {"ready":true} だけが成功。{"error":…} は *errOpencodeInit。それ以外・EOF・時間切れ・
// 上限 (4 KiB) 超過は失敗。この 1 行を読み終えるまで、control には何も送らない。時間切れは、r を閉じて読みを止める (呼び手が teardown する)。
func readReadyLine(r io.ReadCloser, timeout time.Duration) error {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReaderSize(r, opencodeReadyMax).ReadSlice('\n')
		ch <- result{append([]byte(nil), line...), err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-time.After(timeout):
		r.Close()
		return fmt.Errorf("opencode: %v 以内に、init の状態の行が来ない", timeout)
	}
	switch {
	case errors.Is(res.err, io.EOF):
		return errors.New("opencode: init が、状態の行を出さずに終わった")
	case errors.Is(res.err, bufio.ErrBufferFull):
		return fmt.Errorf("opencode: init の状態の行が、%d バイトを超える", opencodeReadyMax)
	case res.err != nil:
		return res.err
	}
	var st struct {
		Ready *bool   `json:"ready"`
		Error *string `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(res.line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return fmt.Errorf("opencode: init の状態の行が読めない: %q", sanitizeStderr(res.line[:min(len(res.line), 120)]))
	}
	switch {
	case st.Ready != nil && *st.Ready && st.Error == nil:
		return nil
	case st.Error != nil && st.Ready == nil:
		return &errOpencodeInit{reason: sanitizeStderr([]byte(*st.Error))}
	}
	return fmt.Errorf("opencode: init の状態の行が、成功でも失敗でもない: %q", sanitizeStderr(res.line[:min(len(res.line), 120)]))
}

// openEventStream は、GET /api/event を、許可リストのヘッダ (Accept・Cache-Control だけ。外れると init が 400 で拒否する。ADR 0030 決定 4) で開き、
// 最初の data: が server.connected であることを、timeout 以内に確かめる。root の session.created を取りこぼさないように、session の作成より前に呼ぶ。
func openEventStream(ctx context.Context, hc *http.Client, timeout time.Duration) (*http.Response, *chat.SSEReader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://opencode.invalid/api/event", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("opencode: GET /api/event: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("opencode: GET /api/event が %d", resp.StatusCode)
	}
	sr := chat.NewSSEReader(resp.Body)
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			data, err := sr.Next()
			if err == chat.ErrLineTooLong || err == chat.ErrBadLine {
				continue
			}
			ch <- result{append([]byte(nil), data...), err}
			return
		}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			resp.Body.Close()
			return nil, nil, fmt.Errorf("opencode: SSE の最初のイベントを読めない: %w", res.err)
		}
		if !chat.IsServerConnected(res.data) {
			resp.Body.Close()
			return nil, nil, errors.New("opencode: SSE の最初のイベントが server.connected でない")
		}
	case <-time.After(timeout):
		resp.Body.Close()
		return nil, nil, fmt.Errorf("opencode: %v 以内に、SSE の server.connected が来ない", timeout)
	}
	return resp, sr, nil
}

// createSession は、POST /api/session で、permissions を固定した session を作り、その ID (opencode.ValidID の形だけ) を返す。2xx でなければ失敗。
func createSession(ctx context.Context, hc *http.Client, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://opencode.invalid/api/session", bytes.NewReader([]byte(opencodeSessionBody)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("opencode: POST /api/session: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, opencodeBodyMax))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("opencode: POST /api/session が %d", resp.StatusCode)
	}
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || !chat.ValidSessionID(r.Data.ID) {
		return "", errors.New("opencode: POST /api/session の応答に、session の id が無い (または形が不正)")
	}
	return r.Data.ID, nil
}

// httpWriter は、会話の指示 (EncodeCommand の 1 行) を、HTTP で実行する io.Writer (QueuedWriter が、1 行ずつ呼ぶ)。行は、chat.ParseHTTPRequest で、
// アダプタとは独立にもう一度検査する。2xx 以外・失敗は error を返す (QueuedWriter の onFail → 会話が終わる。承認の返答が失敗したのに、承認済みの
// ように続くことを避ける)。
type httpWriter struct {
	ctx     context.Context
	hc      *http.Client
	timeout time.Duration
}

func (w *httpWriter) Write(p []byte) (int, error) {
	req, err := chat.ParseHTTPRequest(bytes.TrimSpace(p))
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(w.ctx, w.timeout)
	defer cancel()
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, "http://opencode.invalid"+req.Path, body)
	if err != nil {
		return 0, err
	}
	if req.Body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.hc.Do(hr)
	if err != nil {
		return 0, fmt.Errorf("opencode: %s %s: %w", req.Method, req.Path, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, opencodeBodyMax))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("opencode: %s %s が %d", req.Method, req.Path, resp.StatusCode)
	}
	return len(p), nil
}

// freePort は、127.0.0.1 の空きポートを 1 つ選ぶ (listen して閉じる)。横取りの窓は、init の起動の証明・pidfd・inode が持つ (ADR 0029)。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// startOpencodeChatSession は、cfg の檻 (init --relay-control の下の opencode serve --stdio) を起動し、HTTP の transport で会話を始める。
// cfg の RelayPort・RelayTokenEnv・RelayVersionPrefix は、ここで決める (profile の値と、空きポート)。init が {"error":…} を返したとき
// (ポートが使われていた場合を含む) は、ポートを選び直して 1 回だけ再起動する。
func startOpencodeChatSession(ctx context.Context, id string, cfg cageConfig, allow []string, launch chat.Launch, exeDir string, stderr io.Writer) (*chatSession, error) {
	if cfg.Agent.relayTokenEnv == "" {
		return nil, fmt.Errorf("%s は、HTTP の transport で動かせない (profile に relayTokenEnv が無い)", cfg.Agent.name)
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		port, err := freePort()
		if err != nil {
			return nil, fmt.Errorf("空きポートを選べない: %w", err)
		}
		cfg.RelayPort, cfg.RelayTokenEnv, cfg.RelayVersionPrefix = port, cfg.Agent.relayTokenEnv, cfg.Agent.relayVersionPrefix
		s, err := startOpencodeAttempt(ctx, id, cfg, allow, launch, exeDir, stderr, port)
		if err == nil {
			return s, nil
		}
		var initErr *errOpencodeInit
		if !errors.As(err, &initErr) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func startOpencodeAttempt(ctx context.Context, id string, cfg cageConfig, allow []string, launch chat.Launch, exeDir string, stderr io.Writer, port int) (*chatSession, error) {
	c, err := startCage(ctx, cfg, allow, append(launch.Args(), "--port", strconv.Itoa(port)), exeDir, stderr)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*chatSession, error) {
		c.teardown()
		c.cage.Wait() // 檻 (bwrap) の終了を回収する (cancel で止まる)
		return nil, err
	}
	if err := readReadyLine(c.pair.Out, opencodeReadyTimeout); err != nil {
		return fail(err)
	}
	hctx, hcancel := context.WithCancel(ctx)
	hc := newRelayClient(c.pair.In).HTTPClient() // Timeout は付けない (SSE は長い)。要求ごとの期限は、呼び手が context で持つ
	resp, sr, err := openEventStream(hctx, hc, opencodeConnectTimeout)
	if err != nil {
		hcancel()
		return fail(err)
	}
	created, err := createSession(hctx, hc, opencodeConnectTimeout)
	if err != nil {
		hcancel()
		resp.Body.Close()
		return fail(err)
	}

	s := &chatSession{chatCage: *c, id: id, done: make(chan struct{})}
	s.Chat = chat.NewSession(chat.SessionConfig{
		Launch: launch, ID: id, Input: &httpWriter{ctx: hctx, hc: hc, timeout: opencodeRequestTimeout},
		// 手動の「終了」: control を閉じ (init が子を止める)、檻を止める。
		OnStop: func() {
			c.pair.In.CloseWrite()
			c.cancel()
		},
	})
	readDone := make(chan struct{})
	rootedCh := make(chan struct{})
	var readErr error
	go func() {
		defer close(readDone)
		var rooted atomic.Bool
		timer := time.AfterFunc(opencodeConnectTimeout, func() {
			if !rooted.Load() {
				resp.Body.Close() // root の session.created が来ない: 読みを止める
			}
		})
		defer timer.Stop()
		readErr = s.Chat.ReadSSE(sr, created, func() { rooted.Store(true); close(rootedCh) })
		switch {
		case errors.Is(readErr, chat.ErrRootMismatch):
			fmt.Fprintf(stderr, "goronation serve: opencode の root の session が、作成した session と一致しない。会話を終える\n")
		case !rooted.Load():
			fmt.Fprintf(stderr, "goronation serve: opencode の root の session.created が、%v 以内に来ない。会話を終える\n", opencodeConnectTimeout)
		}
		resp.Body.Close()
		c.cancel() // SSE の終わりは、会話の終わり (再接続しない。ADR 0022 決定 5)
	}()
	go func() {
		<-s.done
		hcancel()
	}()
	go s.run(readDone)
	// root の一致を確かめるまで、会話を渡さない (指示が root の session より先に届いて、失敗するのを避ける。不一致・時間切れは、起動の失敗)。
	select {
	case <-rootedCh:
		return s, nil
	case <-readDone:
	}
	hcancel()
	<-s.done
	if errors.Is(readErr, chat.ErrRootMismatch) {
		return nil, chat.ErrRootMismatch
	}
	return nil, fmt.Errorf("opencode: root の session.created を、%v 以内に確かめられなかった", opencodeConnectTimeout)
}

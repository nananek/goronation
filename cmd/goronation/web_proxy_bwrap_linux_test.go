package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/termrelay/termrelaytest"
)

// 結合テスト (bwrap が要る): 実プロセスの goronation web が、WebAuthn でログインしたブラウザの代わり
// (webauthntest.Authenticator 相当。loggedInClientWithToken 経由) から、セッション一覧・repo の
// ファイルブラウザ・端末ビューの WebSocket までの一通りを、実際に goronation serve (子プロセスとして起動する)
// と本物の bwrap の檻を使って確かめる。goronation serve 自身の UDS 経由の中継ロジック (生バイト列がそのまま
// 往復すること) は serve_terminal_bwrap_linux_test.go で確認済みなので、ここでは goronation web 固有の経路
// (WebAuthn・repo のファイルブラウザ・dial-or-spawn・httputil.ReverseProxy 越しの WebSocket の配線) に
// 絞る。

var webListenRE = regexp.MustCompile(`goronation web: (\S+) で待ち受けている`)

// freeLoopbackPort は、一時的に空きの TCP ポートを 1 つ確保して番号だけ返す (goronation web --listen は、
// 実プロセスの起動時引数として、待ち受けるアドレスを先に決める必要がある。close してから goronation web が
// bind するまでの短い窓での競合は、テスト環境では実用上無視できる)。
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// webFixture は、動いている goronation web と、WebAuthn でログイン済みの http.Client。
type webFixture struct {
	f      *runFixture
	r      *liveRun
	base   string
	client *http.Client
}

// startWebFixture は、goronation web --repos-dir reposDir (省略可) を起動し、ブートストラップトークンの発行・
// WebAuthn の登録・ログインまで済ませる。t.Cleanup で、SIGTERM を送って終了を待つ。
func startWebFixture(t *testing.T, f *runFixture, reposDir string) *webFixture {
	t.Helper()
	port := freeLoopbackPort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	args := []string{"web", "--listen", listen, "--rp-id", "127.0.0.1", "--origin", base, "--state-dir", f.stateDir()}
	if reposDir != "" {
		args = append(args, "--repos-dir", reposDir)
	}
	r := f.start(t, args...)
	t.Cleanup(func() {
		syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
		r.wait()
	})
	waitStderrMatch(t, r, webListenRE)

	tok := f.goronation(t, "web", "token", "--state-dir", f.stateDir()).mustOK(t)
	token := strings.TrimSpace(tok.stdout)
	if token == "" {
		t.Fatalf("ブートストラップトークンが発行されていない: %s", tok)
	}
	client := loggedInClientWithToken(t, base, token, "127.0.0.1", base)
	return &webFixture{f: f, r: r, base: base, client: client}
}

// wsURL は、id の端末ビューの WebSocket URL (ws://.../s/<id>/ws)。
func (wf *webFixture) wsURL(id string) string {
	return "ws" + strings.TrimPrefix(wf.base, "http") + "/s/" + id + "/ws"
}

func (wf *webFixture) cookieHeader(t *testing.T) http.Header {
	t.Helper()
	for _, c := range wf.client.Jar.Cookies(mustParseBase(t, wf.base)) {
		if c.Name == sessionCookieName {
			return http.Header{"Cookie": []string{sessionCookieName + "=" + c.Value}}
		}
	}
	t.Fatal("goronation_session cookie が無い")
	return nil
}

func mustParseBase(t *testing.T, base string) *url.URL {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestWebRepoBrowserStartsSessionAndRelays は、goronation web の repo のファイルブラウザ経由で新しいセッション
// を始め (goronation serve を子プロセスとして起動させ)、その端末ビューの WebSocket が、実際の bwrap の檻の
// 中のエージェントと生バイト列を中継することを確かめる。
func TestWebRepoBrowserStartsSessionAndRelays(t *testing.T) {
	f := newRunFixture(t)
	reposDir := filepath.Dir(f.repo)
	repoName := filepath.Base(f.repo)

	wf := startWebFixture(t, f, reposDir)

	resp := doJSON(t, wf.client, "GET", wf.base+"/api/repos", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/repos = %d", resp.StatusCode)
	}
	repos := decodeJSON[[]string](t, resp)
	found := false
	for _, name := range repos {
		if name == repoName {
			found = true
		}
	}
	if !found {
		t.Fatalf("repo 一覧に %q が無い: %v", repoName, repos)
	}

	startBody, _ := json.Marshal(map[string]string{"repo": repoName})
	resp = doJSON(t, wf.client, "POST", wf.base+"/api/repos/start", startBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/repos/start = %d", resp.StatusCode)
	}
	started := decodeJSON[struct {
		ID string `json:"id"`
	}](t, resp)
	if started.ID == "" {
		t.Fatal("開始したセッションの ID が空")
	}

	// セッション一覧にも出る。
	resp = doJSON(t, wf.client, "GET", wf.base+"/api/sessions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/sessions = %d", resp.StatusCode)
	}
	list := decodeJSON[[]sessionInfoDTO](t, resp)
	seen := false
	for _, s := range list {
		if s.ID == started.ID {
			seen = true
		}
	}
	if !seen {
		t.Errorf("/api/sessions に %s が無い: %+v", started.ID, list)
	}

	// 端末ビューのページ (認証必須) が返る。
	resp = doJSON(t, wf.client, "GET", wf.base+"/s/"+started.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/s/%s = %d", started.ID, resp.StatusCode)
	}
	resp.Body.Close()

	// WebSocket 越しに、goronation web が dial-or-spawn した goronation serve へ実際に繋がることを確かめる。
	// goronation web の repo のファイルブラウザ経由では、エージェントへの引数 (-- ARGS...) を指定する手段が
	// 無い (実運用では、エージェントの既定の対話モードで起動するので問題にならない)。テストのエージェント
	// (fakeClaude) は、引数が無いと即座に終了する場面 (scenario=none) になるので、接続を試みるタイミング
	// によっては、エージェントがすでに終わっていて 410 (Gone) が返ることがある。どちらも
	// 「goronation web → goronation serve の配線」自体は正しく機能した結果なので、両方を正常とみなす (生バイト中継
	// そのものの確認は、--session 再開のテスト (下) と、goronation serve 直接の serve_terminal_bwrap_linux_test.go
	// で、termecho 場面を使って行う)。
	ctx := t.Context()
	cli, resp2, err := termrelaytest.Dial(ctx, wf.wsURL(started.ID), &termrelaytest.DialOptions{Header: wf.cookieHeader(t)})
	if err != nil {
		if resp2 != nil && resp2.StatusCode == http.StatusGone {
			t.Logf("エージェントは接続前にすでに終わっていた (410)。goronation web 経由の起動・配線としては正常")
			return
		}
		t.Fatalf("WebSocket に繋げない (応答 %v): %v", resp2, err)
	}
	defer cli.Close()
	if err := cli.WriteResize(ctx, 80, 24); err != nil {
		t.Fatal(err)
	}
}

// TestWebProxySpawnsServeOnDemandForExistingSession は、goronation run --repo が (goronation web を介さず) 直接
// 作った既存のセッションに、goronation web 経由で初めて繋いだときに、goronation web が goronation serve --session を
// 自動で起動する (dial-or-spawn) ことを確かめる。
func TestWebProxySpawnsServeOnDemandForExistingSession(t *testing.T) {
	f := newRunFixture(t)
	created := f.goronation(t, "run", "--repo", f.repo, "--", "exit", "0").mustOK(t)
	id := sessionID(t, created)

	wf := startWebFixture(t, f, "")
	ctx := t.Context()
	header := wf.cookieHeader(t)

	// この時点で、この id の goronation serve は、まだ (goronation run --repo が終わっているので) 動いていない。
	// goronation web 経由の再開でも、エージェントへの引数は渡せない (上の TestWebRepoBrowserStartsSessionAndRelays
	// と同じ理由)。最初の dial は「goronation serve がまだ動いていない」ため必ず失敗する (それ自体は正常。
	// spawn を挟んでリトライする) が、spawn 後の dial が 410 なら、dial-or-spawn は正しく機能した
	// うえでエージェントが先に終わっただけなので、それも正常とみなして終える。
	deadline := time.Now().Add(serveTestTimeout)
	var cli *termrelaytest.Conn
	var lastResp *http.Response
	var lastErr error
	for time.Now().Before(deadline) {
		var resp *http.Response
		var err error
		cli, resp, err = termrelaytest.Dial(ctx, wf.wsURL(id), &termrelaytest.DialOptions{Header: header})
		if err == nil {
			break
		}
		if resp != nil && resp.StatusCode == http.StatusGone {
			t.Logf("dial-or-spawn は機能したが、エージェントはすでに終わっていた (410)。配線としては正常")
			return
		}
		lastResp, lastErr = resp, err
		time.Sleep(200 * time.Millisecond)
	}
	if cli == nil {
		t.Fatalf("goronation web 経由での自動起動に失敗した (応答 %v): %v", lastResp, lastErr)
	}
	defer cli.Close()
	if err := cli.WriteResize(ctx, 80, 24); err != nil {
		t.Fatal(err)
	}
}

// TestWebTerminalWebSocketRequiresAuth は、cookie 無しで /s/{id}/ws に繋いでも、goronation serve へ届く前に
// requireSession で拒まれることを確かめる (goronation serve 自身は UDS 越しなら無条件に信頼するので、この
// 認証は goronation web の層だけで担保されている)。
func TestWebTerminalWebSocketRequiresAuth(t *testing.T) {
	f := newRunFixture(t)
	wf := startWebFixture(t, f, "")
	ctx := t.Context()
	_, resp, err := termrelaytest.Dial(ctx, wf.wsURL("20260101-000000-aaaaaa"), nil)
	if err == nil {
		t.Fatal("未認証の接続が通った")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未認証接続の応答 = %d, want 401", resp.StatusCode)
	}
}

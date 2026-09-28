package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/session"
	iwebauthn "github.com/nananek/goronation/cmd/internal/webauthn"
	"github.com/nananek/goronation/cmd/internal/webauthn/webauthntest"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// このファイルは、goronation web の HTTP ハンドラの配線 (routing・cookie・状態コード) を、実際に HTTP で叩いて
// 確かめる結合テスト (bwrap は要らない: goronation serve の起動が絡む部分は web_proxy_bwrap_linux_test.go)。
// WebAuthn 自体の検証ロジック (署名・origin・challenge など) の単体テストは、cmd/internal/webauthn 側に
// ある。ここでは、本物のブラウザの代わりに、webauthntest.Authenticator (偽の認証器) が、実際の
// PublicKeyCredential の応答と同じ形の JSON を組み立てて送る。

// newTestServer は、httptest.Server を、その URL 自体を Config.Origin にして起こす (WebAuthn は origin の
// 完全一致を求めるので、動的なポートを、後から埋める必要がある。http.HandlerFunc 経由の間接呼び出しで、
// サーバーの URL が分かってから、本物のハンドラを差し込む)。origin は "http://127.0.0.1:PORT" の形になる
// (Config.Validate が、http を許す唯一の例外)。reposDir は空 (--repos-dir 未指定と同じ)。
func newTestServer(t *testing.T) (srv *httptest.Server, store *iwebauthn.Store, rpID, origin string) {
	t.Helper()
	srv, store, _, rpID, origin = newTestServerWithRepos(t, "")
	return srv, store, rpID, origin
}

// newTestServerWithRepos は、newTestServer と同じだが、reposDir (--repos-dir 相当) を指定できる。
func newTestServerWithRepos(t *testing.T, reposDir string) (srv *httptest.Server, store *iwebauthn.Store, sessStore *session.Store, rpID, origin string) {
	t.Helper()
	stateDir := t.TempDir()
	store, err := iwebauthn.NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sessStore, err = session.NewStore(stateDir, bwrap.CurrentHost())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	origin = srv.URL
	rpID = "127.0.0.1"
	cfg := iwebauthn.Config{RPID: rpID, RPName: "test", Origin: origin}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("testConfig を作れない: %v", err)
	}
	handler = newWebMux(cfg, store, origin, sessStore, reposDir, stateDir, self)
	return srv, store, sessStore, rpID, origin
}

// doJSON は、srv へ JSON の POST/GET を送る。cookies があれば、それを一緒に送る。
func doJSON(t *testing.T, client *http.Client, method, url string, body []byte) *http.Response {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

type optionsAndState struct {
	Options struct {
		Challenge string `json:"challenge"`
	} `json:"options"`
	State string `json:"state"`
}

// loggedInClient は、register → login まで済ませた http.Client を返す。
func loggedInClient(t *testing.T, srv *httptest.Server, store *iwebauthn.Store, rpID, origin string) *http.Client {
	t.Helper()
	tok, err := iwebauthn.IssueBootstrapToken(t.Context(), store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return loggedInClientWithToken(t, srv.URL, tok, rpID, origin)
}

// loggedInClientWithToken は、baseURL (goronation web の origin) へ、すでに発行済みの token (goronation web token
// でも iwebauthn.IssueBootstrapToken でもよい) を使って register → login まで済ませた http.Client を
// 返す。実プロセスの goronation web (結合テスト、bwrap 要) と、httptest.Server 越しの goronation web (単体テスト)
// の、どちらからも呼べる (トークンの発行手段だけが違うので、それを呼び手が分ける)。
func loggedInClientWithToken(t *testing.T, baseURL, token, rpID, origin string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	cred, err := webauthntest.New()
	if err != nil {
		t.Fatal(err)
	}
	beginBody, _ := json.Marshal(map[string]string{"token": token})
	resp := doJSON(t, client, "POST", baseURL+"/webauthn/register/begin", beginBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/begin = %d", resp.StatusCode)
	}
	begin := decodeJSON[optionsAndState](t, resp)
	attResp, err := cred.Register(rpID, origin, begin.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	finishBody, _ := json.Marshal(map[string]any{"state": begin.State, "credential": attResp})
	resp = doJSON(t, client, "POST", baseURL+"/webauthn/register/finish", finishBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, client, "POST", baseURL+"/webauthn/login/begin", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/begin = %d", resp.StatusCode)
	}
	lb := decodeJSON[optionsAndState](t, resp)
	assResp, err := cred.Authenticate(rpID, origin, lb.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	lfBody, _ := json.Marshal(map[string]any{"state": lb.State, "credential": assResp})
	resp = doJSON(t, client, "POST", baseURL+"/webauthn/login/finish", lfBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()
	return client
}

// TestWebFullFlow は、token → register → login → whoami (200) → logout → whoami (401) を、実際に
// HTTP で確かめる。
func TestWebFullFlow(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := loggedInClient(t, srv, store, rpID, origin)

	resp := doJSON(t, client, "GET", srv.URL+"/api/whoami", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ログイン後の whoami = %d, want 200", resp.StatusCode)
	}
	who := decodeJSON[map[string]bool](t, resp)
	if !who["authenticated"] {
		t.Errorf("whoami = %+v", who)
	}

	resp = doJSON(t, client, "POST", srv.URL+"/logout", nil)
	resp.Body.Close()
	resp = doJSON(t, client, "GET", srv.URL+"/api/whoami", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logout 後の whoami = %d, want 401", resp.StatusCode)
	}
}

func TestWebRegisterBeginRejectsBadToken(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	body, _ := json.Marshal(map[string]string{"token": "not-a-real-token"})
	resp := doJSON(t, client, "POST", srv.URL+"/webauthn/register/begin", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("code = %d, want 403", resp.StatusCode)
	}
}

func TestWebLoginBeginRejectsBeforeRegistration(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	resp := doJSON(t, client, "POST", srv.URL+"/webauthn/login/begin", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("code = %d, want 404", resp.StatusCode)
	}
}

func TestWebWhoamiRequiresCookie(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	resp := doJSON(t, client, "GET", srv.URL+"/api/whoami", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", resp.StatusCode)
	}
}

func TestWebIndexAndAppJS(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	for _, path := range []string{"/", "/static/app.js"} {
		resp := doJSON(t, client, "GET", srv.URL+path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestWebSecurityHeaders(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	resp := doJSON(t, client, "GET", srv.URL+"/", nil)
	defer resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", resp.Header.Get("X-Content-Type-Options"))
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("Content-Security-Policy が無い")
	}
}

// TestWebTerminalPageAndVendorAssets は、セッション一覧・端末ビューのページ (認証必須) と、埋め込んだ
// vendor の静的アセット (認証不要) が、それぞれ想定どおりの状態コード・Content-Type で返ることを
// 確かめる。
func TestWebTerminalPageAndVendorAssets(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}

	for _, path := range []string{"/sessions", "/s/20260101-000000-aaaaaa"} {
		resp := doJSON(t, client, "GET", srv.URL+path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("未認証の %s = %d, want 401", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	for path, wantType := range map[string]string{
		"/static/sessions.js":         "text/javascript",
		"/static/terminal.js":         "text/javascript",
		"/static/terminal.css":        "text/css",
		"/static/vendor/xterm.js":     "text/javascript",
		"/static/vendor/xterm.css":    "text/css",
		"/static/vendor/addon-fit.js": "text/javascript",
	} {
		resp := doJSON(t, client, "GET", srv.URL+path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s の Content-Type = %q, want %q で始まる", path, ct, wantType)
		}
		resp.Body.Close()
	}
}

// TestWebTerminalPageHasNoInlineStyle は、Issue #39 の回帰テスト。端末ビューのページ (/s/{id}) は
// CSP (style-src 'self') の下でインライン <style> を使うと描画領域が潰れて画面が真っ黒になる。
// terminalHTML の応答本文にインライン <style> が含まれないこと (/static/terminal.css を経由して
// 読んでいること) を、CSP ヘッダーが緩められていないことと合わせて確かめる。
func TestWebTerminalPageHasNoInlineStyle(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := loggedInClient(t, srv, store, rpID, origin)

	resp := doJSON(t, client, "GET", srv.URL+"/s/20260101-000000-aaaaaa", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("認証済みの /s/{id} = %d, want 200", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self'") {
		t.Errorf("Content-Security-Policy = %q, want style-src 'self' を含む (緩めていないこと)", csp)
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.String(), "<style") {
		t.Errorf("端末ビューのページにインライン <style> が残っている (CSP style-src 'self' でブロックされる): %s", body.String())
	}
}

// cspNonceFromHeader は、Content-Security-Policy ヘッダーの値から 'nonce-<値>' の <値> を取り出す
// (無ければ空文字)。
func cspNonceFromHeader(csp string) string {
	const marker = "'nonce-"
	i := strings.Index(csp, marker)
	if i < 0 {
		return ""
	}
	rest := csp[i+len(marker):]
	j := strings.IndexByte(rest, '\'')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// cspNonceFromMeta は、端末ビューの HTML 本文から <meta name="csp-nonce" content="<値>"> の <値> を
// 取り出す (無ければ空文字)。
func cspNonceFromMeta(body string) string {
	const marker = `<meta name="csp-nonce" content="`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestWebTerminalPageCSPNonceMatchesMeta は、PR② (terminal-page-csp-nonce) の中心の回帰テスト。
// handleTerminalPage が発行する nonce が、CSP ヘッダー (style-src 'self' 'nonce-<値>') と、応答本文の
// <meta name="csp-nonce" content="<値>"> (terminal.js がここから読んで xterm.js の cspNonce オプション
// に渡す) とで、一致すること・空でないことを確かめる。
func TestWebTerminalPageCSPNonceMatchesMeta(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := loggedInClient(t, srv, store, rpID, origin)

	resp := doJSON(t, client, "GET", srv.URL+"/s/20260101-000000-aaaaaa", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("認証済みの /s/{id} = %d, want 200", resp.StatusCode)
	}

	headerNonce := cspNonceFromHeader(resp.Header.Get("Content-Security-Policy"))
	if headerNonce == "" {
		t.Fatalf("Content-Security-Policy に 'nonce-<値>' が無い: %q", resp.Header.Get("Content-Security-Policy"))
	}

	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	metaNonce := cspNonceFromMeta(body.String())
	if metaNonce == "" {
		t.Fatalf("応答本文に <meta name=\"csp-nonce\" content=\"...\"> が無い")
	}

	if headerNonce != metaNonce {
		t.Errorf("CSP ヘッダーの nonce (%q) と <meta> の nonce (%q) が一致しない", headerNonce, metaNonce)
	}
}

// TestWebTerminalPageCSPNoncePerRequest は、nonce がリクエストごとに新しく発行され、使い回されない
// ことを確かめる (使い回すと、漏れた 1 つの nonce が以後のリクエストにも効いてしまう)。
func TestWebTerminalPageCSPNoncePerRequest(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := loggedInClient(t, srv, store, rpID, origin)

	var nonces []string
	for i := 0; i < 2; i++ {
		resp := doJSON(t, client, "GET", srv.URL+"/s/20260101-000000-aaaaaa", nil)
		nonce := cspNonceFromHeader(resp.Header.Get("Content-Security-Policy"))
		resp.Body.Close()
		if nonce == "" {
			t.Fatalf("%d 回目: nonce が空", i)
		}
		nonces = append(nonces, nonce)
	}
	if nonces[0] == nonces[1] {
		t.Errorf("2 回のリクエストで同じ nonce (%q) が返った。リクエストごとに新しく発行されていない", nonces[0])
	}
}

// TestWebOtherRoutesHaveNoNonce は、nonce 付き CSP が /s/{id} だけの上書きであり、他のルートの既定の
// CSP (securityHeaders) に漏れ出していないことを確かめる。
func TestWebOtherRoutesHaveNoNonce(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := loggedInClient(t, srv, store, rpID, origin)

	for _, path := range []string{"/", "/sessions", "/static/terminal.js"} {
		resp := doJSON(t, client, "GET", srv.URL+path, nil)
		csp := resp.Header.Get("Content-Security-Policy")
		resp.Body.Close()
		if strings.Contains(csp, "nonce-") {
			t.Errorf("%s の Content-Security-Policy に nonce が漏れている (/s/{id} 限定のはず): %q", path, csp)
		}
	}
}

// TestWebTerminalJSReadsCspNonceMeta は、terminal.js が <meta name="csp-nonce"> を読み、xterm.js の
// Terminal オプション cspNonce へ渡す配線 (PR②) の回帰テスト。実際のブラウザでの DOM 反映は確認でき
// ないため、配信された terminal.js の本文に、meta タグの読み取りと cspNonce への受け渡しのコードが
// 含まれていることを確かめる。
func TestWebTerminalJSReadsCspNonceMeta(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}

	resp := doJSON(t, client, "GET", srv.URL+"/static/terminal.js", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/static/terminal.js = %d, want 200", resp.StatusCode)
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), `meta[name="csp-nonce"]`) {
		t.Error("terminal.js が <meta name=\"csp-nonce\"> を読んでいない")
	}
	if !strings.Contains(body.String(), "cspNonce:") {
		t.Error("terminal.js が Terminal オプションに cspNonce を渡していない")
	}
}

// TestWebVendorXtermSupportsCspNonceOption は、Issue #46 (CSP 下で xterm.js の動的 <style> がブロック
// される) 対応の一部 (PR①、cmd/goronation/vendor/xterm/PATCH.md) の回帰テスト。vendor 済み xterm.js が
// `cspNonce` Terminal オプションと、それを <style> 要素に適用する内部メソッドを含んでいることを、配信
// された本文から確かめる (ブラウザを起動しての実際の DOM 検証はできないため、パッチが取り除かれて
// pristine な公式ビルドに巻き戻ってしまう退行を文字列の存在で検知する)。
// nonce を実際にサーバーが生成して Terminal に渡す配線 (PR②、TestWebTerminalPageCSPNonceMatchesMeta・
// TestWebTerminalJSReadsCspNonceMeta) とは別の観点 (vendor 済み xterm.js 自体の対応) を見る。
func TestWebVendorXtermSupportsCspNonceOption(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}

	resp := doJSON(t, client, "GET", srv.URL+"/static/vendor/xterm.js", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/static/vendor/xterm.js = %d, want 200", resp.StatusCode)
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), "cspNonce") {
		t.Error("vendor 済み xterm.js に cspNonce オプションが見当たらない (PATCH.md のパッチが失われていないか確認する)")
	}
}

// TestWebSessionsListRequiresAuth・TestWebSessionsListEmpty は、GET /api/sessions の認証・応答の形を
// 確かめる (実際にセッションを作る結合テストは web_proxy_bwrap_linux_test.go)。
func TestWebSessionsListRequiresAuth(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	resp := doJSON(t, client, "GET", srv.URL+"/api/sessions", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", resp.StatusCode)
	}
}

func TestWebSessionsListEmpty(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := loggedInClient(t, srv, store, rpID, origin)
	resp := doJSON(t, client, "GET", srv.URL+"/api/sessions", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d, want 200", resp.StatusCode)
	}
	list := decodeJSON[[]sessionInfoDTO](t, resp)
	if len(list) != 0 {
		t.Errorf("list = %+v, want 空", list)
	}
}

// TestWebReposListWithoutReposDir は、--repos-dir を指定しないと、/api/repos が空の一覧を返す (機能
// 自体が無いだけで、エラーにはしない) ことを確かめる。
func TestWebReposListWithoutReposDir(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := loggedInClient(t, srv, store, rpID, origin)
	resp := doJSON(t, client, "GET", srv.URL+"/api/repos", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d, want 200", resp.StatusCode)
	}
	list := decodeJSON[[]string](t, resp)
	if len(list) != 0 {
		t.Errorf("list = %+v, want 空", list)
	}
}

// TestWebReposListEnumeratesGitReposOnly は、--repos-dir 直下のディレクトリのうち、.git を持つものだけが
// 列挙されることを確かめる (深い階層はたどらない)。
func TestWebReposListEnumeratesGitReposOnly(t *testing.T) {
	reposDir := t.TempDir()
	for _, name := range []string{"repo-a", "repo-b", "not-a-repo"} {
		if err := os.MkdirAll(filepath.Join(reposDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"repo-a", "repo-b"} {
		if err := os.MkdirAll(filepath.Join(reposDir, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 深い階層 (repo-a/nested) は、直下には出ない。
	if err := os.MkdirAll(filepath.Join(reposDir, "repo-a", "nested", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv, store, _, rpID, origin := newTestServerWithRepos(t, reposDir)
	client := loggedInClient(t, srv, store, rpID, origin)
	resp := doJSON(t, client, "GET", srv.URL+"/api/repos", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d, want 200", resp.StatusCode)
	}
	list := decodeJSON[[]string](t, resp)
	if len(list) != 2 || list[0] != "repo-a" || list[1] != "repo-b" {
		t.Errorf("list = %+v, want [repo-a repo-b]", list)
	}
}

// TestWebRepoStartRejectsBadName は、POST /api/repos/start が、path traversal を試みる名前を断ることを
// 確かめる (実際にセッションを開始する結合テストは web_proxy_bwrap_linux_test.go)。
func TestWebRepoStartRejectsBadName(t *testing.T) {
	reposDir := t.TempDir()
	srv, store, _, rpID, origin := newTestServerWithRepos(t, reposDir)
	client := loggedInClient(t, srv, store, rpID, origin)
	for _, name := range []string{"../escape", "/etc/passwd", "", "a/b"} {
		body, _ := json.Marshal(map[string]string{"repo": name})
		resp := doJSON(t, client, "POST", srv.URL+"/api/repos/start", body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("repo=%q が受理された", name)
		}
	}
}

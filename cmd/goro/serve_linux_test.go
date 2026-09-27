package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	iwebauthn "github.com/nananek/goronation/cmd/internal/webauthn"
	"github.com/nananek/goronation/cmd/internal/webauthn/webauthntest"
)

// このファイルは、goro serve の HTTP ハンドラの配線 (routing・cookie・状態コード) を、実際に HTTP で叩いて
// 確かめる結合テスト。WebAuthn 自体の検証ロジック (署名・origin・challenge など) の単体テストは、
// cmd/internal/webauthn 側にある。ここでは、本物のブラウザの代わりに、webauthntest.Authenticator (偽の
// 認証器) が、実際の PublicKeyCredential の応答と同じ形の JSON を組み立てて送る (cbor は、この package では
// 直接 import しない。ADR 0004・tools/archtest の webauthn-only-dep が、それを強制する)。

// newTestServer は、httptest.Server を、その URL 自体を Config.Origin にして起こす (WebAuthn は origin の
// 完全一致を求めるので、動的なポートを、後から埋める必要がある。http.HandlerFunc 経由の間接呼び出しで、
// サーバーの URL が分かってから、本物のハンドラを差し込む)。origin は "http://127.0.0.1:PORT" の形になる
// (Config.Validate が、http を許す唯一の例外)。
func newTestServer(t *testing.T) (srv *httptest.Server, store *iwebauthn.Store, rpID, origin string) {
	t.Helper()
	store, err := iwebauthn.NewStore(t.TempDir())
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
	handler = newServeMux(cfg, store, origin)
	return srv, store, rpID, origin
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

// TestServeFullFlow は、token → register → login → whoami (200) → logout → whoami (401) を、実際に
// HTTP で確かめる。
func TestServeFullFlow(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	tok, err := iwebauthn.IssueBootstrapToken(t.Context(), store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := webauthntest.New()
	if err != nil {
		t.Fatal(err)
	}

	// register/begin
	beginBody, _ := json.Marshal(map[string]string{"token": tok})
	resp := doJSON(t, client, "POST", srv.URL+"/webauthn/register/begin", beginBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/begin = %d", resp.StatusCode)
	}
	begin := decodeJSON[optionsAndState](t, resp)

	// register/finish
	attResp, err := cred.Register(rpID, origin, begin.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	registerFinishBody, _ := json.Marshal(map[string]any{"state": begin.State, "credential": attResp})
	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/register/finish", registerFinishBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// whoami はまだログインしていない。
	resp = doJSON(t, client, "GET", srv.URL+"/api/whoami", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ログイン前の whoami = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// login/begin
	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/login/begin", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/begin = %d", resp.StatusCode)
	}
	loginBegin := decodeJSON[optionsAndState](t, resp)

	// login/finish
	assResp, err := cred.Authenticate(rpID, origin, loginBegin.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	loginFinishBody, _ := json.Marshal(map[string]any{"state": loginBegin.State, "credential": assResp})
	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/login/finish", loginFinishBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// whoami はログイン後、200 になる (cookie が自動で付く)。
	resp = doJSON(t, client, "GET", srv.URL+"/api/whoami", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ログイン後の whoami = %d, want 200", resp.StatusCode)
	}
	who := decodeJSON[map[string]bool](t, resp)
	if !who["authenticated"] {
		t.Errorf("whoami = %+v", who)
	}

	// logout の後は、また 401 になる。
	resp = doJSON(t, client, "POST", srv.URL+"/logout", nil)
	resp.Body.Close()
	resp = doJSON(t, client, "GET", srv.URL+"/api/whoami", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logout 後の whoami = %d, want 401", resp.StatusCode)
	}
}

func TestServeRegisterBeginRejectsBadToken(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	body, _ := json.Marshal(map[string]string{"token": "not-a-real-token"})
	resp := doJSON(t, client, "POST", srv.URL+"/webauthn/register/begin", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("code = %d, want 403", resp.StatusCode)
	}
}

func TestServeLoginBeginRejectsBeforeRegistration(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	resp := doJSON(t, client, "POST", srv.URL+"/webauthn/login/begin", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("code = %d, want 404", resp.StatusCode)
	}
}

func TestServeWhoamiRequiresCookie(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	client := &http.Client{}
	resp := doJSON(t, client, "GET", srv.URL+"/api/whoami", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", resp.StatusCode)
	}
}

func TestServeIndexAndAppJS(t *testing.T) {
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

func TestServeSecurityHeaders(t *testing.T) {
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

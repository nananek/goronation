package main

// 攻撃者視点レビュー (OpenCode による独立レビュー attack-review-28d386c-opencode と、それを検証した
// attack-review-28d386c-judged) が見つけた・確認したセッション周りの再現テスト。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	iwebauthn "github.com/nananek/goronation/cmd/internal/webauthn"
	"github.com/nananek/goronation/cmd/internal/webauthn/webauthntest"
)

// attackRegisterAndLogin は、偽認証器で登録→ログインまで通し、login/finish の応答を返す。
func attackRegisterAndLogin(t *testing.T) (srvURL string, loginResp *http.Response, client *http.Client) {
	t.Helper()
	srv, store, rpID, origin := newTestServer(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client = &http.Client{Jar: jar}
	tok, err := iwebauthn.IssueBootstrapToken(t.Context(), store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := webauthntest.New()
	if err != nil {
		t.Fatal(err)
	}
	beginBody, _ := json.Marshal(map[string]string{"token": tok})
	resp := doJSON(t, client, "POST", srv.URL+"/webauthn/register/begin", beginBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/begin = %d", resp.StatusCode)
	}
	begin := decodeJSON[optionsAndState](t, resp)
	attResp, err := cred.Register(rpID, origin, begin.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	finishBody, _ := json.Marshal(map[string]any{"state": begin.State, "credential": attResp})
	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/register/finish", finishBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/login/begin", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/begin = %d", resp.StatusCode)
	}
	lb := decodeJSON[optionsAndState](t, resp)
	assResp, err := cred.Authenticate(rpID, origin, lb.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	lfBody, _ := json.Marshal(map[string]any{"state": lb.State, "credential": assResp})
	loginResp = doJSON(t, client, "POST", srv.URL+"/webauthn/login/finish", lfBody)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login/finish = %d", loginResp.StatusCode)
	}
	return srv.URL, loginResp, client
}

// TestAttackCookieAttributes は、セッション cookie の属性と、token が本文に漏れないことを確かめる
// (記録用。誇張した書き方をしないための、独立レビューの再確認)。
func TestAttackCookieAttributes(t *testing.T) {
	_, loginResp, _ := attackRegisterAndLogin(t)
	defer loginResp.Body.Close()
	raw := loginResp.Header.Values("Set-Cookie")
	if len(raw) != 1 {
		t.Fatalf("Set-Cookie が %d 本: %v", len(raw), raw)
	}
	cs := loginResp.Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookie が %d 個", len(cs))
	}
	c := cs[0]
	if !c.HttpOnly {
		t.Error("HttpOnly が無い")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q", c.Path)
	}
	if c.MaxAge != int(iwebauthn.SessionTTL.Seconds()) {
		t.Errorf("Max-Age = %d, want %d", c.MaxAge, int(iwebauthn.SessionTTL.Seconds()))
	}
	if strings.Contains(raw[0], "Secure") {
		t.Error("http origin なのに Secure が付いている (ブラウザが保存できない)")
	}
	body, err := io.ReadAll(loginResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), c.Value) {
		t.Error("セッション token が応答本文に漏れている")
	}
}

// TestAttackLogoutDoesNotRevokeStolenSession は、logout がサーバー側でセッションを失効させること
// (奪った cookie が logout 後は使えなくなること) を確かめる。攻撃者視点レビューが最初に見つけたときは、
// handleLogout が cookie を消すだけで、stateless なセッション token を失効させる仕組みが無く、盗まれた
// cookie が有効期限 (30日) いっぱい通ってしまっていた。VerifySession が persistedState.SessionEpoch との
// 一致を要求し、Logout がそれを進めるようになったことで、logout 以前に発行された token は、logout した
// 本人の cookie を含めて全て失効する (1 ユーザー・1 セッション前提の設計なので、他の正当なブラウザを
// 巻き込む心配は無い)。
func TestAttackLogoutDoesNotRevokeStolenSession(t *testing.T) {
	srvURL, loginResp, client := attackRegisterAndLogin(t)
	cookies := loginResp.Cookies()
	loginResp.Body.Close()
	var session string
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("セッション cookie が無い")
	}
	resp := doJSON(t, client, "POST", srvURL+"/logout", nil)
	resp.Body.Close()

	req, err := http.NewRequest("GET", srvURL+"/api/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	stolen := &http.Client{}
	resp2, err := stolen.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logout 後も、奪った cookie が %d で通る (サーバー側の失効が無い)", resp2.StatusCode)
	}
}

// TestAttackNoOriginCheckOnPost は、POST に Origin の検査が無いこと (SameSite 頼みであること) を記録する
// (Limit。CSRF 耐性は cookie の SameSite=Strict と、token を cookie でなく本文で運ぶ設計で保たれている)。
func TestAttackNoOriginCheckOnPost(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	req, err := http.NewRequest("POST", srv.URL+"/webauthn/register/begin", strings.NewReader(`{"token":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	t.Logf("クロスオリジン (Origin: https://evil.example)・Content-Type: text/plain の POST への応答: %d (Origin ヘッダは検査されていない)", resp.StatusCode)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("想定外の状態コード: %d", resp.StatusCode)
	}
}

// TestAttackRegisterBeginRegistrationOracle は、トークンを持たない攻撃者でも、409 と 403 の違いから
// 「すでに登録済みか」を判別できることを確かめる (Limit。実害は小さいと判断済み)。
func TestAttackRegisterBeginRegistrationOracle(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	client := &http.Client{}
	body, _ := json.Marshal(map[string]string{"token": "definitely-wrong-token"})

	resp := doJSON(t, client, "POST", srv.URL+"/webauthn/register/begin", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未登録での不正トークン = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	cred, err := webauthntest.New()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := iwebauthn.IssueBootstrapToken(t.Context(), store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	beginBody, _ := json.Marshal(map[string]string{"token": tok})
	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/register/begin", beginBody)
	begin := decodeJSON[optionsAndState](t, resp)
	attResp, err := cred.Register(rpID, origin, begin.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	finishBody, _ := json.Marshal(map[string]any{"state": begin.State, "credential": attResp})
	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/register/finish", finishBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, client, "POST", srv.URL+"/webauthn/register/begin", body)
	defer resp.Body.Close()
	t.Logf("登録済み + 不正トークン = %d (409 なら、トークン無しでも登録済みかどうかが分かる)", resp.StatusCode)
}

// TestUnauthenticatedThirdPartyCanForceLogoutActiveSession は、セッション cookie を一切持たない
// 第三者が POST /logout を叩いても、他人の稼働中セッションを強制失効させられないことを確かめる。
//
// L2 の修正 (Logout が SessionEpoch を進め、発行済みの全セッション token を一括で失効させる) を
// 最初に入れたとき、handleLogout が requireSession 相当の検証をせず、iwebauthn.Logout(ctx, store)
// 自身も呼び手を一切確認しない設計だったため、盗む必要すら無く、認証情報を一切持たない誰かが
// POST /logout を無制限に繰り返すだけで、正規利用者を任意のタイミングで強制ログアウトさせられる、
// という新しい (B1 の接続保持型 DoS よりもさらに安価な) 穴になっていた。攻撃者視点レビューの指摘で
// 見つかり、handleLogout が有効なセッションを提示できた場合だけ iwebauthn.Logout を呼ぶように
// 直した。
func TestUnauthenticatedThirdPartyCanForceLogoutActiveSession(t *testing.T) {
	srvURL, loginResp, victimClient := attackRegisterAndLogin(t)
	loginResp.Body.Close()

	// 被害者 (victimClient) が、ログイン直後にまだ何もしていない時点で、whoami が通ることを確認する
	// (前提の確認)。
	resp := doJSON(t, victimClient, "GET", srvURL+"/api/whoami", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("前提が崩れている: ログイン直後の whoami = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// 攻撃者: セッション cookie を一切持たない、別の http.Client (cookie jar 無し) で、
	// POST /logout を叩く。
	attacker := &http.Client{}
	attackResp := doJSON(t, attacker, "POST", srvURL+"/logout", nil)
	attackResp.Body.Close()
	if attackResp.StatusCode != http.StatusOK {
		t.Errorf("認証情報を一切持たない第三者の POST /logout への応答 = %d, want 200 (状態の違いを応答で漏らさない)", attackResp.StatusCode)
	}

	// 被害者の (盗まれてすらいない、本人の手元にある) セッション cookie が、まだ有効かを確認する。
	resp2 := doJSON(t, victimClient, "GET", srvURL+"/api/whoami", nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("認証情報を一切持たない第三者が POST /logout を叩くだけで、被害者の正規のアクティブセッションが強制失効させられた (whoami = %d, want 200)", resp2.StatusCode)
	}
}

// TestLogoutInvalidatesOtherLegitimateSessions は、同じユーザーが複数タブ/デバイスでログインしている
// とき、片方の logout がもう片方の (正当な) セッションも失効させることを確かめる (記録用。1 ユーザー・
// 1 credential 前提の設計では、セッション単位ではなく世代単位で一括失効するのは意図どおりの挙動と判断
// した。バグではない)。
func TestLogoutInvalidatesOtherLegitimateSessions(t *testing.T) {
	srv, store, rpID, origin := newTestServer(t)
	tok, err := iwebauthn.IssueBootstrapToken(t.Context(), store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := webauthntest.New()
	if err != nil {
		t.Fatal(err)
	}
	beginBody, _ := json.Marshal(map[string]string{"token": tok})
	jarA, _ := cookiejar.New(nil)
	clientA := &http.Client{Jar: jarA}
	resp := doJSON(t, clientA, "POST", srv.URL+"/webauthn/register/begin", beginBody)
	begin := decodeJSON[optionsAndState](t, resp)
	attResp, err := cred.Register(rpID, origin, begin.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	finishBody, _ := json.Marshal(map[string]any{"state": begin.State, "credential": attResp})
	resp = doJSON(t, clientA, "POST", srv.URL+"/webauthn/register/finish", finishBody)
	resp.Body.Close()

	loginAs := func(client *http.Client) {
		r := doJSON(t, client, "POST", srv.URL+"/webauthn/login/begin", nil)
		lb := decodeJSON[optionsAndState](t, r)
		assResp, err := cred.Authenticate(rpID, origin, lb.Options.Challenge)
		if err != nil {
			t.Fatal(err)
		}
		lfBody, _ := json.Marshal(map[string]any{"state": lb.State, "credential": assResp})
		r2 := doJSON(t, client, "POST", srv.URL+"/webauthn/login/finish", lfBody)
		r2.Body.Close()
	}
	// タブ A・タブ B (同じユーザー、別々のセッション token) で、それぞれログインする。
	loginAs(clientA)
	jarB, _ := cookiejar.New(nil)
	clientB := &http.Client{Jar: jarB}
	loginAs(clientB)

	// どちらも whoami が通る。
	for name, c := range map[string]*http.Client{"A": clientA, "B": clientB} {
		r := doJSON(t, c, "GET", srv.URL+"/api/whoami", nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("タブ %s のログイン直後の whoami = %d", name, r.StatusCode)
		}
		r.Body.Close()
	}
	// タブ A が logout する。
	r := doJSON(t, clientA, "POST", srv.URL+"/logout", nil)
	r.Body.Close()

	// タブ B (別デバイス相当) のセッションも失効しているはず (世代単位の一括失効。1 ユーザー前提の
	// 設計では、意図どおりと考えられる)。
	r = doJSON(t, clientB, "GET", srv.URL+"/api/whoami", nil)
	defer r.Body.Close()
	if r.StatusCode == http.StatusOK {
		t.Error("タブ A の logout 後も、タブ B のセッションが有効なまま (世代が個別に管理されている?)")
	}
}

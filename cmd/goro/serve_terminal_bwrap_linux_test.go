package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/termrelay/termrelaytest"
	"github.com/nananek/goronation/cmd/internal/webauthn/webauthntest"
)

// 結合テスト (bwrap が要る): 実プロセスの goro serve が、WebAuthn でログインしたブラウザの代わり
// (webauthntest.Authenticator・termrelaytest) から、実際の bwrap の檻の中の偽エージェント (termecho
// 場面) へ、生バイト列を中継することを確かめる。pty 中継そのもの (SIGWINCH・raw モードなど) は
// run_bwrap_linux_test.go (TestRunPtyPropagatesWinsize 等) が既に確認済みなので、ここでは
// goro serve 固有の経路 (WebAuthn 認証・WebSocket・termSession の viewer 管理) に絞る。

const serveTestTimeout = 90 * time.Second

var serveListenRE = regexp.MustCompile(`goro serve: (\S+) で待ち受けている`)

// freeLoopbackPort は、テストのために、一時的に listen してすぐ閉じる loopback のポート。goro serve の
// --origin は起動時の固定の引数なので、port 0 の自動割り当て後に埋めることができず、先に確保しておく
// 必要がある (確保後、実際に bind するまでの間に、別のプロセスが同じポートを奪う理論上の余地はあるが、
// この検証環境で現実的なリスクではない)。
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitStderrMatch は、r の標準エラー出力が re に一致するまで待ち、一致部分を返す。
func waitStderrMatch(t *testing.T, r *liveRun, re *regexp.Regexp) []string {
	t.Helper()
	deadline := time.Now().Add(serveTestTimeout)
	for {
		if m := re.FindStringSubmatch(r.stderr.String()); m != nil {
			return m
		}
		select {
		case <-r.done:
			t.Fatalf("%s が出る前に、goro serve が終わった: %v\nstderr:\n%s", re, r.err, r.stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s が出ない\nstderr:\n%s", re, r.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serveTermFixture は、動いている goro serve と、WebAuthn でログイン済みの http.Client。
type serveTermFixture struct {
	f      *runFixture
	r      *liveRun
	base   string // "http://127.0.0.1:PORT"
	client *http.Client
}

// startServeTermFixture は、goro serve (termecho 場面) を起動し、ブートストラップトークンの発行・
// WebAuthn の登録・ログインまで済ませる。sessionArgs は、セッションの選び方 ({"--repo", f.repo} か
// {"--session", id})。sessionRE は、起動後の案内 (「を作った」か「を再開した」) の確認に使う。
// t.Cleanup で、SIGTERM を送って終了を待つ。
func startServeTermFixture(t *testing.T, f *runFixture, sessionArgs []string, sessionRE *regexp.Regexp) *serveTermFixture {
	t.Helper()
	port := freeLoopbackPort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	args := append([]string{"serve", "--listen", listen, "--rp-id", "127.0.0.1", "--origin", base, "--state-dir", f.stateDir()}, sessionArgs...)
	args = append(args, "--", "termecho")
	r := f.start(t, args...)
	t.Cleanup(func() {
		syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
		r.wait()
	})
	waitStderrMatch(t, r, serveListenRE)
	waitStderrMatch(t, r, sessionRE)

	tok := f.goro(t, "serve", "token", "--state-dir", f.stateDir()).mustOK(t)
	token := strings.TrimSpace(tok.stdout)
	if token == "" {
		t.Fatalf("ブートストラップトークンが発行されていない: %s", tok)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	cred, err := webauthntest.New()
	if err != nil {
		t.Fatal(err)
	}
	const rpID = "127.0.0.1"
	beginBody, _ := json.Marshal(map[string]string{"token": token})
	resp := doJSON(t, client, "POST", base+"/webauthn/register/begin", beginBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/begin = %d", resp.StatusCode)
	}
	begin := decodeJSON[optionsAndState](t, resp)
	attResp, err := cred.Register(rpID, base, begin.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	finishBody, _ := json.Marshal(map[string]any{"state": begin.State, "credential": attResp})
	resp = doJSON(t, client, "POST", base+"/webauthn/register/finish", finishBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doJSON(t, client, "POST", base+"/webauthn/login/begin", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/begin = %d", resp.StatusCode)
	}
	lb := decodeJSON[optionsAndState](t, resp)
	assResp, err := cred.Authenticate(rpID, base, lb.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	lfBody, _ := json.Marshal(map[string]any{"state": lb.State, "credential": assResp})
	resp = doJSON(t, client, "POST", base+"/webauthn/login/finish", lfBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/finish = %d", resp.StatusCode)
	}
	resp.Body.Close()

	return &serveTermFixture{f: f, r: r, base: base, client: client}
}

// wsURL は、base (http://...) を、ws:// の /ws/terminal URL にする。
func (sf *serveTermFixture) wsURL() string {
	return "ws" + strings.TrimPrefix(sf.base, "http") + "/ws/terminal"
}

// sessionCookie は、client が持つ、goro_session cookie の値。
func (sf *serveTermFixture) sessionCookie(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(sf.base)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range sf.client.Jar.Cookies(u) {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	t.Fatal("goro_session cookie が無い")
	return ""
}

// TestServeTerminalRelaysRawBytes は、WebAuthn で認証したセッションだけが端末ビューの WebSocket に
// 繋がり、生バイト列 (中身を一切解釈・加工しない) が、実際の bwrap の檻の中のエージェントとの間で
// 往復することを確かめる。未認証の接続の拒否・2 本目の接続による置き換え・エージェント終了後の 410 も
// あわせて確認する。
func TestServeTerminalRelaysRawBytes(t *testing.T) {
	f := newRunFixture(t)
	sf := startServeTermFixture(t, f, []string{"--repo", f.repo}, regexp.MustCompile(`セッション \S+ を作った`))
	ctx := t.Context()

	// (1) 未認証 (cookie 無し) の接続は拒否される (requireSession が、WebSocket の Accept より前で断る)。
	if _, resp, err := termrelaytest.Dial(ctx, sf.wsURL(), nil); err == nil {
		t.Fatal("未認証の WebSocket 接続が通った")
	} else if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未認証接続の応答 = %d, want 401", resp.StatusCode)
	}

	// (2) 認証済み接続: "ready" (fake agent の起動) が、生バイトのまま届く。
	cookie := sf.sessionCookie(t)
	header := http.Header{"Cookie": []string{sessionCookieName + "=" + cookie}}
	cli1, _, err := termrelaytest.Dial(ctx, sf.wsURL(), header)
	if err != nil {
		t.Fatal(err)
	}
	defer cli1.Close()
	if err := cli1.WriteResize(ctx, 100, 40); err != nil { // resize (テキストフレーム) を混ぜても壊れない
		t.Fatal(err)
	}
	// "ready" (fake agent の起動直後の出力) は、待たない: goro serve --repo は、WS 接続より前 (起動時)
	// に檻を起こすので、接続した時点ですでに出力済み・誰も見ていなかった分として捨てられている可能性が
	// 高い (goro-serve-plan §0-3 の「誰も見ていない間の出力は、貯めずに捨てる」設計どおり)。入力は
	// pty の buffer に積まれるので、この後の書き込み (エコーの確認) はタイミングに依らず届く。

	// (3) 送った生バイト列が、加工されずにそのまま返る (エコーの場面。goro は中身を解釈しない)。
	const probe = "hello, goro serve terminal! \x1b[31mred\x1b[0m 日本語\r\n"
	if err := cli1.WriteBinary(ctx, []byte(probe)); err != nil {
		t.Fatal(err)
	}
	readUntil(t, cli1, probe)

	// (4) 2 本目の接続は、1 本目を置き換える (「端末がついてくる」設計)。1 本目は閉じられる。
	cli2, _, err := termrelaytest.Dial(ctx, sf.wsURL(), header)
	if err != nil {
		t.Fatal(err)
	}
	defer cli2.Close()
	if _, err := cli1.ReadBinary(ctx); err == nil {
		t.Error("置き換えられたはずの 1 本目が、まだ読めている")
	}
	const probe2 = "second viewer\n"
	if err := cli2.WriteBinary(ctx, []byte(probe2)); err != nil {
		t.Fatal(err)
	}
	readUntil(t, cli2, probe2)

	// (5) エージェントを終了させる (0x04 = Ctrl-D/EOT。termecho 場面の終了条件)。以後、新しい接続は 410。
	if err := cli2.WriteBinary(ctx, []byte{0x04}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(serveTestTimeout)
	for {
		_, _, err := termrelaytest.Dial(ctx, sf.wsURL(), header)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("セッション終了後も、新しい接続が 410 にならない")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServeTerminalResumesSession は、goro serve --session ID (前に goro run --repo が作ったセッション
// の再開) が、goro serve --repo と同じ経路 (prepareAgentLaunch・resolveRepoTarget) で正しく配線されて
// おり、実際に端末ビューの WebSocket が使えることを確かめる。
func TestServeTerminalResumesSession(t *testing.T) {
	f := newRunFixture(t)
	created := f.goro(t, "run", "--repo", f.repo, "--", "exit", "0").mustOK(t)
	id := sessionID(t, created)

	sf := startServeTermFixture(t, f, []string{"--session", id}, regexp.MustCompile(`セッション \S+ を再開した`))
	ctx := t.Context()
	cookie := sf.sessionCookie(t)
	header := http.Header{"Cookie": []string{sessionCookieName + "=" + cookie}}
	cli, _, err := termrelaytest.Dial(ctx, sf.wsURL(), header)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	const probe = "resumed\n"
	if err := cli.WriteBinary(ctx, []byte(probe)); err != nil {
		t.Fatal(err)
	}
	readUntil(t, cli, probe)
}

// readUntil は、cli から読み続け、連結した内容が want を含むまで待つ (境界がバイト単位で保証されない
// ため、1 回の ReadBinary が want と一致するとは限らない)。
func readUntil(t *testing.T, cli *termrelaytest.Conn, want string) {
	t.Helper()
	var got strings.Builder
	deadline := time.Now().Add(serveTestTimeout)
	for !strings.Contains(got.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%q が届かない (got %q)", want, got.String())
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		b, err := cli.ReadBinary(ctx)
		cancel()
		if err != nil {
			continue
		}
		got.Write(b)
	}
}

// termReadUntil は、readUntil と同じことを、蓄積したバイト列 ([]byte) をそのまま返す形でする
// (攻撃者視点レビューの再現テストが、受け取った生バイト列そのものを検査するのに使う)。
func termReadUntil(t *testing.T, cli *termrelaytest.Conn, want string, timeout time.Duration) []byte {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(timeout)
	for {
		if bytes.Contains(got, []byte(want)) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q が届かない (got %q)", want, got)
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		b, err := cli.ReadBinary(ctx)
		cancel()
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		got = append(got, b...)
	}
}

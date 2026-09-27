package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/goronation/egress/git"
)

// fakeGit は、偽の git 上流。受けた要求を記録し、canned の応答を返す。
type fakeGit struct {
	mu           sync.Mutex
	hits         []fakeGitHit
	advOK        []byte // info/refs の応答本文
	rpOK         []byte // receive-pack の応答本文 (report-status のふり)
	upOK         []byte // upload-pack の応答本文
	status       int    // 0 なら 200
	redirect     string // 空でなければ、この URL へ 302 で redirect する
	chunkedBytes int    // 0 より大きければ、advOK の代わりに、この長さを Content-Length 無しで (chunked で) 書く
}

type fakeGitHit struct {
	Method, Path, Query, ContentType, Auth, GitProtocol string
	Body                                                []byte
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		advOK: []byte("001e# service=git-receive-pack\n0000"),
		rpOK:  []byte("0011\x01000eunpack ok\n0019ok refs/heads/x\n00000000"),
		upOK:  []byte("PACK-ish-response-body"),
	}
}

func (f *fakeGit) Server(t testing.TB) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, fakeGitHit{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, ContentType: r.Header.Get("Content-Type"), Auth: r.Header.Get("Authorization"), GitProtocol: r.Header.Get("Git-Protocol"), Body: body})
		f.mu.Unlock()
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusFound)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if f.chunkedBytes > 0 {
			fl, _ := w.(http.Flusher)
			for i := 0; i < f.chunkedBytes; i += 64 {
				w.Write(bytes.Repeat([]byte{'x'}, 64))
				if fl != nil {
					fl.Flush() // Content-Length を確定させず、chunked にする
				}
			}
			return
		}
		switch {
		case r.Method == "GET":
			w.Write(f.advOK)
		case strings.HasSuffix(r.URL.Path, "git-receive-pack"):
			w.Write(f.rpOK)
		default:
			w.Write(f.upOK)
		}
	}))
}

func (f *fakeGit) Hits() []fakeGitHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeGitHit(nil), f.hits...)
}

func gitHandler(t testing.TB, gitBase string, src *staticSource) *Handler {
	t.Helper()
	cfg := testConfig(t, git.Repo{Owner: "o", Name: "r"}, testSession)
	cfg.GitBaseURL = gitBase
	cfg.Credentials = src
	return newHandler(t, cfg)
}

const prefix = git.PathPrefix + "o/r.git/"

func TestServeGitAdvertise(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	w := doRequest(t, h, "GET", prefix+"info/refs?service=git-upload-pack", nil, nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), up.advOK) {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.Bytes())
	}
	hits := up.Hits()
	if len(hits) != 1 || hits[0].Method != "GET" || hits[0].Path != "/o/r.git/info/refs" || hits[0].Query != "service=git-upload-pack" {
		t.Fatalf("%+v", hits)
	}
	if hits[0].Auth == "" || !bytes.Contains([]byte(hits[0].Auth), []byte("Basic")) {
		t.Errorf("Authorization が付いていない: %q", hits[0].Auth)
	}
}

func TestServeGitReceivePackForwardsRawBytes(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	for _, name := range []string{"create", "update", "multi", "atomic", "big", "nested"} {
		t.Run(name, func(t *testing.T) {
			body := fixture(t, name)
			w := doRequest(t, h, "POST", prefix+"git-receive-pack", body, map[string]string{"Content-Type": "application/x-git-receive-pack-request"})
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			hits := up.Hits()
			last := hits[len(hits)-1]
			if !bytes.Equal(last.Body, body) {
				t.Fatalf("上流に届いたバイト列が、要求と一致しない (%d vs %d バイト)", len(last.Body), len(body))
			}
			if last.ContentType != "application/x-git-receive-pack-request" {
				t.Errorf("Content-Type = %q", last.ContentType)
			}
		})
	}
}

func TestServeGitReceivePackRejectsOutOfPolicy(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	cases := map[string]int{
		"delete": 403, "tag": 403, "lighttag": 403, "main": 403, "mixed": 403,
		"pushopt": 400, "shallow": 400, "signed": 400, "unicode": 400, "sha256": 400,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			body := fixture(t, name)
			w := doRequest(t, h, "POST", prefix+"git-receive-pack", body, nil)
			if w.Code != want {
				t.Fatalf("status=%d, 期待 %d (body=%s)", w.Code, want, w.Body)
			}
		})
	}
	if len(up.Hits()) != 0 {
		t.Fatalf("拒否した要求が、上流に届いた: %d 件", len(up.Hits()))
	}
}

func TestServeGitReceivePackProbe(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	body := fixture(t, "big-probe")
	w := doRequest(t, h, "POST", prefix+"git-receive-pack", body, nil)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	hits := up.Hits()
	if len(hits) != 1 || !bytes.Equal(hits[0].Body, body) {
		t.Fatalf("探りが、そのまま上流に届いていない: %+v", hits)
	}
}

func TestServeGitContentEncodingRejected(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	w := doRequest(t, h, "POST", prefix+"git-receive-pack", fixture(t, "create"), map[string]string{"Content-Encoding": "gzip"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d", w.Code)
	}
	if len(up.Hits()) != 0 {
		t.Fatal("Content-Encoding つきの要求が、上流に届いた")
	}
}

func TestServeGitOtherRepoRejected(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	for _, target := range []string{
		git.PathPrefix + "x/y.git/info/refs?service=git-upload-pack",
		git.PathPrefix + "x/y.git/git-receive-pack",
		git.PathPrefix + "x/y.git/git-upload-pack",
	} {
		w := doRequest(t, h, methodFor(target), target, []byte{}, nil)
		if w.Code != 403 {
			t.Errorf("%s: status=%d", target, w.Code)
		}
	}
	if len(up.Hits()) != 0 {
		t.Fatalf("別の repo への要求が、上流に届いた: %d 件", len(up.Hits()))
	}
}

func methodFor(target string) string {
	if bytes.Contains([]byte(target), []byte("info/refs")) {
		return "GET"
	}
	return "POST"
}

func TestServeGitPackTooLarge(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	cfg := testConfig(t, git.Repo{Owner: "o", Name: "r"}, testSession)
	cfg.GitBaseURL, cfg.Credentials, cfg.MaxPackBytes = srv.URL, src, 300 // "big" フィクスチャは 512 バイト
	h := newHandler(t, cfg)

	full := fixture(t, "big")
	w := doRequest(t, h, "POST", prefix+"git-receive-pack", full, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	// 大きさを事前に知らずに中継するため、上限を超える前の分は、既に送り始めていることがある。
	// それでも、全体 (上限を超えた分を含む) が、丸ごと届くことは無い。
	for _, hit := range up.Hits() {
		if len(hit.Body) >= len(full) {
			t.Fatalf("上限を超えた pack が、そのまま上流に届いた (%d バイト)", len(hit.Body))
		}
	}
}

func TestServeGitCredentialMissing(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{} // ok=false: 常に ErrNotFound
	h := gitHandler(t, srv.URL, src)

	w := doRequest(t, h, "GET", prefix+"info/refs?service=git-upload-pack", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", w.Code)
	}
	if len(up.Hits()) != 0 {
		t.Fatal("資格情報が無いのに、上流に接続した")
	}
	if bytes.Contains(w.Body.Bytes(), []byte(testToken)) {
		t.Fatal("トークンが応答に漏れた")
	}
}

func TestServeGitNoRedirect(t *testing.T) {
	allowLoopback(t)
	target := newFakeGit() // リダイレクトの先 (追ってしまうと、ここが応答する)
	targetSrv := target.Server(t)
	defer targetSrv.Close()

	up := newFakeGit()
	up.redirect = targetSrv.URL + "/x"
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	w := doRequest(t, h, "GET", prefix+"info/refs?service=git-upload-pack", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("リダイレクトを追った・別の状態: status=%d body=%s", w.Code, w.Body)
	}
	if len(target.Hits()) != 0 {
		t.Fatalf("リダイレクトの先に、実際に届いた (追ってしまった): %d 件", len(target.Hits()))
	}
}

func TestServeGitDialCheckIsWired(t *testing.T) {
	// allowLoopback を呼ばない: 既定 (egress.ForbiddenAddr) のまま、127.0.0.1 (httptest) への接続が、実際に断られることを確かめる。
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	w := doRequest(t, h, "GET", prefix+"info/refs?service=git-upload-pack", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("dial-check が効いていない: status=%d body=%s", w.Code, w.Body)
	}
	if len(up.Hits()) != 0 {
		t.Fatal("禁止した IP なのに、上流に届いた")
	}
}

// TestServeGitPackTooLargeDuringCommands は、コマンド部そのものを読んでいる間 (pkt-line のパース中) に、大きさの上限を
// 超えたときも、413 になることを確かめる (relayGit の client.Do の間に超える TestServeGitPackTooLarge とは別の経路)。
func TestServeGitPackTooLargeDuringCommands(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	cfg := testConfig(t, git.Repo{Owner: "o", Name: "r"}, testSession)
	cfg.GitBaseURL, cfg.Credentials, cfg.MaxPackBytes = srv.URL, src, 10 // "create" の 1 行目より、ずっと小さい
	h := newHandler(t, cfg)

	w := doRequest(t, h, "POST", prefix+"git-receive-pack", fixture(t, "create"), nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(up.Hits()) != 0 {
		t.Fatal("コマンド部すら読み切れない要求が、上流に届いた")
	}
}

// TestServeGitResponseSizeCapped は、上流が Content-Length を宣言せず (chunked)、上限を超える量を送っても、
// 檻に渡す量が、上限を超えないことを確かめる。
func TestServeGitResponseSizeCapped(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	up.chunkedBytes = 2000
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	cfg := testConfig(t, git.Repo{Owner: "o", Name: "r"}, testSession)
	cfg.GitBaseURL, cfg.Credentials, cfg.MaxPackBytes = srv.URL, src, 100
	h := newHandler(t, cfg)

	w := doRequest(t, h, "GET", prefix+"info/refs?service=git-upload-pack", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Body.Len() > 100 {
		t.Fatalf("応答が、上限 (100) を超えて、檻に渡った: %d バイト", w.Body.Len())
	}
}

// TestServeGitGitProtocolHeader は、Git-Protocol ヘッダが、決めた 2 つの値だけ、上流に転送されることを確かめる。
func TestServeGitGitProtocolHeader(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	for _, tc := range []struct{ sent, want string }{
		{"version=2", "version=2"}, {"version=1", "version=1"}, {"version=99", ""}, {"version=2; extra", ""}, {"", ""},
	} {
		doRequest(t, h, "GET", prefix+"info/refs?service=git-upload-pack", nil, map[string]string{"Git-Protocol": tc.sent})
		hits := up.Hits()
		last := hits[len(hits)-1]
		if last.GitProtocol != tc.want {
			t.Errorf("Git-Protocol %q を送ったら、上流には %q が届くはず (実際 %q)", tc.sent, tc.want, last.GitProtocol)
		}
	}
}

// TestServeGitCommandSectionTooLarge は、http.MaxBytesReader の上限 (h.maxPack) に届く前に、egress/git 自身の
// コマンド部の上限 (git.Limits{} の既定 16 KiB) を超えたときも、413 になることを確かめる (gitStatus(git.CodeTooLarge)
// の経路。TestServeGitPackTooLarge・…DuringCommands は、どちらも *http.MaxBytesError の経路で、この経路を通らない)。
func TestServeGitCommandSectionTooLarge(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src) // MaxPackBytes は既定 (512 MiB) のまま: http 側の上限では、まだ切れない

	// 1 個の pkt-line で、宣言する長さだけを大きくする (中身は "old new ref" の形でなくてよい: 中身を見る前に、
	// 大きさの予算で断られる)。
	huge := "4e20" + strings.Repeat("x", 0x4e20-4) // 0x4e20 = 20000 バイト (16 KiB の予算を、これ 1 本で超える)
	w := doRequest(t, h, "POST", prefix+"git-receive-pack", []byte(huge), nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(up.Hits()) != 0 {
		t.Fatal("コマンド部の予算を超えた要求が、上流に届いた")
	}
}

func TestServeGitUpstreamErrorStatus(t *testing.T) {
	allowLoopback(t)
	up := newFakeGit()
	up.status = 500
	srv := up.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(t, srv.URL, src)

	w := doRequest(t, h, "GET", prefix+"info/refs?service=git-upload-pack", nil, nil)
	if w.Code != 500 {
		t.Fatalf("上流の状態コードを、そのまま伝えるはず: %d", w.Code)
	}
}

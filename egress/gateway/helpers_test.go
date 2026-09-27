package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nananek/goronation/core/credential"
	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

const (
	testSession = "20260927-041500-a1b2c3"
	testToken   = "github_pat_test-token-value"
)

// fixture は、PR ① (egress/git) が採取した、実物の git 2.47.3 の receive-pack の本文を読む。
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "git", "testdata", "capture", "git-2.47.3", name+".bin"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// staticSource は、固定のトークンを返す credential.Source (テスト用)。ok が false なら、常に credential.ErrNotFound。
type staticSource struct {
	name  string
	token string
	ok    bool

	mu    sync.Mutex
	calls int
}

func (s *staticSource) Token(ctx context.Context, name string) (credential.Secret, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if err := credential.CheckName(name); err != nil {
		return credential.Secret{}, err
	}
	if !s.ok || name != s.name {
		return credential.Secret{}, credential.ErrNotFound
	}
	return credential.New(s.token), nil
}

func (s *staticSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// allowLoopback は、テストの間、forbiddenAddr を「何も禁止しない」にする (httptest.Server は loopback のため)。
// t.Cleanup で、本番の判定 (egress.ForbiddenAddr) に必ず戻す。
func allowLoopback(t testing.TB) {
	t.Helper()
	orig := forbiddenAddr
	forbiddenAddr = func(netip.Addr) bool { return false }
	t.Cleanup(func() { forbiddenAddr = orig })
}

// testConfig は、repo・セッションから、テスト用の Config の骨組みを作る (GitBaseURL・APIBaseURL・Credentials は、呼び手が足す)。
func testConfig(t testing.TB, repo git.Repo, session string) Config {
	t.Helper()
	push, err := git.NewPolicy(repo, session)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Push: push, Pull: github.PullPolicy{Push: push}}
}

// newHandler は、cfg から Handler を作る (New の error を、テストの失敗にする)。
func newHandler(t testing.TB, cfg Config) *Handler {
	t.Helper()
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// doRequest は、h に、httptest.NewRecorder で要求を送る (net には出ない)。
func doRequest(t testing.TB, h http.Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

// rawRequest は、method・target (RequestURI) を、そのまま (url.Parse などの検査を経ずに) 持つ *http.Request を作る。
// httptest.NewRequest は、target を url.ParseRequestURI に通すため、壊れた target で panic する。ここでは、生の
// 文字列を、Handler がどう扱うかだけを見たいので、骨組み (Header など) だけ NewRequest から借りて、直接差し替える。
func rawRequest(method, target string, body []byte) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Method = method
	r.RequestURI = target
	r.ContentLength = int64(len(body))
	r.Body = io.NopCloser(bytes.NewReader(body))
	return r
}

var fuzzRepo = git.Repo{Owner: "o", Name: "r"}

// FuzzServeGitAuthorizedOnly は、任意の method・target で、Handler がパニックせず、許可した repo (o/r) 以外への
// 要求が、1 件も上流に届かないことを確かめる (攻撃者視点レビュー L3: CheckRepo を呼び忘れた入口を作らない、の配線側の証拠)。
// body は空にする (receive-pack は、空の本文では常に断られるので、期待は Op != OpReceivePack のときだけ)。
func FuzzServeGitAuthorizedOnly(f *testing.F) {
	for _, s := range []struct{ method, target string }{
		{"GET", git.PathPrefix + "o/r.git/info/refs?service=git-upload-pack"},
		{"GET", git.PathPrefix + "o/r.git/info/refs?service=git-receive-pack"},
		{"POST", git.PathPrefix + "o/r.git/git-upload-pack"},
		{"POST", git.PathPrefix + "o/r.git/git-receive-pack"},
		{"GET", git.PathPrefix + "x/y.git/info/refs?service=git-upload-pack"},
		{"POST", git.PathPrefix + "x/y.git/git-receive-pack"},
		{"GET", git.PathPrefix + "O/R.git/info/refs?service=git-upload-pack"}, // 大文字小文字違いは、許可内
		{"GET", git.PathPrefix + "o/r.git/../x.git/info/refs?service=git-upload-pack"},
		{"GET", "/git/github.com.evil/o/r.git/info/refs?service=git-upload-pack"},
		{"POST", git.PathPrefix + "o/r.git/git-receive-pack?x=1"},
		{"TRACE", git.PathPrefix + "o/r.git/info/refs?service=git-upload-pack"},
		{"GET", ""},
		{"", ""},
	} {
		f.Add(s.method, s.target)
	}
	allowLoopback(f)
	up := newFakeGit()
	srv := up.Server(f)
	f.Cleanup(srv.Close)
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := gitHandler(f, srv.URL, src)

	f.Fuzz(func(t *testing.T, method, target string) {
		before := len(up.Hits())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, rawRequest(method, target, nil))
		if w.Code < 100 || w.Code > 599 {
			t.Fatalf("不正な状態コード: %d (%s %q)", w.Code, method, target)
		}
		route, err := git.ParseRoute(method, target)
		authorized := err == nil && route.Repo.Equal(fuzzRepo) && route.Op != git.OpReceivePack
		if !authorized && len(up.Hits()) > before {
			t.Fatalf("許可外の要求 (%s %q) が、上流に届いた", method, target)
		}
	})
}

// FuzzServePullAuthorizedOnly は、任意の method・target・本文で、許可した repo (o/r) 以外への PR 作成が、
// 1 件も上流に届かないことを確かめる。
func FuzzServePullAuthorizedOnly(f *testing.F) {
	okBody := `{"title":"t","head":"goro/` + testSession + `/x","base":"main"}`
	for _, s := range []struct{ method, target, body string }{
		{"POST", github.PathPrefix + "repos/o/r/pulls", okBody},
		{"POST", github.PathPrefix + "repos/x/y/pulls", okBody},
		{"POST", github.PathPrefix + "repos/O/R/pulls", okBody},
		{"GET", github.PathPrefix + "repos/o/r/pulls", ""},
		{"POST", github.PathPrefix + "repos/o/r/pulls/1", okBody},
		{"POST", github.PathPrefix + "repos/o/r/pulls", `{"title":"t","head":"main"}`},
		{"POST", github.PathPrefix + "repos/o/r/pulls", ""},
		{"POST", "/github-api/repos/o/r/pulls", "{}"},
		{"", "", ""},
	} {
		f.Add(s.method, s.target, s.body)
	}
	allowLoopback(f)
	api := newFakeAPI()
	srv := api.Server(f)
	f.Cleanup(srv.Close)
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(f, srv.URL, src)

	f.Fuzz(func(t *testing.T, method, target, body string) {
		before := len(api.Hits())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, rawRequest(method, target, []byte(body)))
		if w.Code < 100 || w.Code > 599 {
			t.Fatalf("不正な状態コード: %d (%s %q)", w.Code, method, target)
		}
		repo, err := github.ParsePullRoute(method, target)
		authorized := err == nil && repo.Equal(fuzzRepo)
		if !authorized && len(api.Hits()) > before {
			t.Fatalf("許可外の要求 (%s %q %q) が、上流に届いた", method, target, body)
		}
	})
}

// FuzzServeHTTPNoPanic は、任意の method・target・本文・少数のヘッダで、ServeHTTP がパニックしないことを確かめる
// (git・github どちらの経路にも、上流 (偽) は無く、error になっても、そこで止まる)。
func FuzzServeHTTPNoPanic(f *testing.F) {
	for _, s := range []string{"", "/", git.PathPrefix, github.PathPrefix, git.PathPrefix + "o/r.git/git-receive-pack", github.PathPrefix + "repos/o/r/pulls"} {
		f.Add("POST", s, []byte("0000"))
		f.Add("GET", s, []byte(""))
	}
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	cfg := testConfig(f, fuzzRepo, testSession)
	cfg.Credentials = src
	// 127.0.0.1 は、既定の forbiddenAddr (egress.ForbiddenAddr、この fuzz では差し替えない) が dial-check で断るので、
	// 実際にはどこにも接続しに行かない (198.51.100.1 のような、本物の未使用アドレスだと、実接続を試みて遅くなる)。
	cfg.GitBaseURL, cfg.APIBaseURL = "http://127.0.0.1:1", "http://127.0.0.1:1"
	h := newHandler(f, cfg)
	f.Fuzz(func(t *testing.T, method, target string, body []byte) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, rawRequest(method, target, body))
	})
}

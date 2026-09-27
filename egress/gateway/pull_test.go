package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

type fakePullHit struct {
	Method, Path, Auth, Body string
}

// fakeAPI は、偽の api.github.com。/repos/o/r/pulls への POST と、/repos/o/r への GET (既定の branch) を受ける。
type fakeAPI struct {
	mu            sync.Mutex
	hits          []fakePullHit
	pullStatus    int    // 0 なら 201
	pullBody      string // 空なら、要求の title・head から作った canned な成功応答
	repoStatus    int
	defaultBranch string
}

func newFakeAPI() *fakeAPI { return &fakeAPI{defaultBranch: "main"} }

func (f *fakeAPI) Server(t testing.TB) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, fakePullHit{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: string(body)})
		f.mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls") == false && !strings.Contains(r.URL.Path, "/pulls"):
			status := f.repoStatus
			if status == 0 {
				status = 200
			}
			w.WriteHeader(status)
			if status == 200 {
				json.NewEncoder(w).Encode(map[string]string{"default_branch": f.defaultBranch})
			}
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			status := f.pullStatus
			if status == 0 {
				status = 201
			}
			w.WriteHeader(status)
			if f.pullBody != "" {
				w.Write([]byte(f.pullBody))
				return
			}
			var req map[string]any
			json.Unmarshal(body, &req)
			num := 5
			w.Write([]byte(`{"number":` + strconv.Itoa(num) + `,"html_url":"https://github.com` + strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos"), "/pulls") + `/pull/` + strconv.Itoa(num) + `","draft":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func (f *fakeAPI) Hits() []fakePullHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePullHit(nil), f.hits...)
}

func pullHandler(t testing.TB, apiBase string, src *staticSource) *Handler {
	t.Helper()
	cfg := testConfig(t, git.Repo{Owner: "o", Name: "r"}, testSession)
	cfg.APIBaseURL = apiBase
	cfg.Credentials = src
	return newHandler(t, cfg)
}

const pullTarget = github.PathPrefix + "repos/o/r/pulls"

func TestServePullCreatesDraft(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(t, srv.URL, src)

	body := []byte(`{"title":"t","head":"goro/` + testSession + `/x","base":"main","draft":false}`)
	w := doRequest(t, h, "POST", pullTarget, body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	hits := api.Hits()
	if len(hits) != 1 || hits[0].Path != "/repos/o/r/pulls" {
		t.Fatalf("%+v", hits)
	}
	if !strings.Contains(hits[0].Auth, "Bearer "+testToken) {
		t.Errorf("Authorization = %q", hits[0].Auth)
	}
	// 上流に送った本文は、draft: false を上書きした、作り直した JSON。
	if !strings.Contains(hits[0].Body, `"draft":true`) {
		t.Fatalf("draft が強制されていない: %s", hits[0].Body)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["number"] == nil || resp["html_url"] == nil || len(resp) != 2 {
		t.Fatalf("檻に返す応答が、number・html_url だけではない: %v", resp)
	}
}

func TestServePullDefaultBranch(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	api.defaultBranch = "develop"
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(t, srv.URL, src)

	body := []byte(`{"title":"t","head":"goro/` + testSession + `/x"}`)
	w := doRequest(t, h, "POST", pullTarget, body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	hits := api.Hits()
	if len(hits) != 2 {
		t.Fatalf("既定の branch を引く GET が無い: %+v", hits)
	}
	if !strings.Contains(hits[1].Body, `"base":"develop"`) {
		t.Fatalf("既定の branch が使われていない: %s", hits[1].Body)
	}
}

func TestServePullRejectsOutOfPolicy(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(t, srv.URL, src)

	cases := map[string]int{
		`{"title":"t","head":"main","base":"main"}`:                                        403,
		`{"title":"t","head":"goro/other-session/x","base":"main"}`:                        403,
		`{"head":"goro/` + testSession + `/x","base":"main"}`:                              400,
		`{"title":"` + strings.Repeat("a", 300) + `","head":"goro/` + testSession + `/x"}`: 400,
		`{"Title":"t","head":"goro/` + testSession + `/x"}`:                                400,
	}
	for body, want := range cases {
		w := doRequest(t, h, "POST", pullTarget, []byte(body), nil)
		if w.Code != want {
			t.Errorf("%s: status=%d, 期待 %d", body, w.Code, want)
		}
	}
	if len(api.Hits()) != 0 {
		t.Fatalf("拒否した要求が、上流に届いた: %d 件", len(api.Hits()))
	}
}

func TestServePullOtherRepoRejected(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(t, srv.URL, src)

	target := github.PathPrefix + "repos/x/y/pulls"
	w := doRequest(t, h, "POST", target, []byte(`{"title":"t","head":"goro/`+testSession+`/x","base":"main"}`), nil)
	if w.Code != 403 {
		t.Fatalf("status=%d", w.Code)
	}
	if len(api.Hits()) != 0 {
		t.Fatal("別の repo への PR が、上流に届いた")
	}
}

func TestServePullQuota(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	cfg := testConfig(t, git.Repo{Owner: "o", Name: "r"}, testSession)
	cfg.APIBaseURL, cfg.Credentials, cfg.PRQuota = srv.URL, src, 2
	h := newHandler(t, cfg)

	body := []byte(`{"title":"t","head":"goro/` + testSession + `/x","base":"main"}`)
	for i := 0; i < 2; i++ {
		w := doRequest(t, h, "POST", pullTarget, body, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("[%d] status=%d", i, w.Code)
		}
	}
	w := doRequest(t, h, "POST", pullTarget, body, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3 回目: status=%d", w.Code)
	}
	if len(api.Hits()) != 2 {
		t.Fatalf("枠を超えた要求が、上流に届いた: %d 件", len(api.Hits()))
	}
}

func TestServePullUpstreamRejects(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	api.pullStatus = 422
	api.pullBody = `{"message":"Validation Failed"}`
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(t, srv.URL, src)

	body := []byte(`{"title":"t","head":"goro/` + testSession + `/x","base":"main"}`)
	w := doRequest(t, h, "POST", pullTarget, body, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestServePullBadUpstreamResponse(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	api.pullBody = `{"number":5,"html_url":"https://github.com/x/y/pull/5"}` // 別の repo の URL
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(t, srv.URL, src)

	body := []byte(`{"title":"t","head":"goro/` + testSession + `/x","base":"main"}`)
	w := doRequest(t, h, "POST", pullTarget, body, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestServePullCredentialMissing(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{}
	h := pullHandler(t, srv.URL, src)

	body := []byte(`{"title":"t","head":"goro/` + testSession + `/x","base":"main"}`)
	w := doRequest(t, h, "POST", pullTarget, body, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", w.Code)
	}
	if len(api.Hits()) != 0 {
		t.Fatal("資格情報が無いのに、上流に接続した")
	}
}

func TestServePullDefaultBranchFailure(t *testing.T) {
	allowLoopback(t)
	api := newFakeAPI()
	api.repoStatus = 404
	srv := api.Server(t)
	defer srv.Close()
	src := &staticSource{name: CredentialName, token: testToken, ok: true}
	h := pullHandler(t, srv.URL, src)

	body := []byte(`{"title":"t","head":"goro/` + testSession + `/x"}`)
	w := doRequest(t, h, "POST", pullTarget, body, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", w.Code)
	}
	for _, hit := range api.Hits() {
		if strings.HasSuffix(hit.Path, "/pulls") {
			t.Fatal("既定の branch を引けないのに、PR を作ろうとした")
		}
	}
}

package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/goronation/core/credential"
)

// fakeUpstream は、GraphQL の偽の上流。query の中身 (mutation か query か) で応答を切り替える。
type fakeUpstream struct {
	mu        sync.Mutex
	mutations int // markReadyForReview が呼ばれた回数
	// handleQuery/handleMutation が、応答の本文 (JSON) を返す。nil なら既定の成功応答。
	pullRequestResp func() string
	mutationResp    func() string
	authHeader      string // 最後に受けた Authorization ヘッダ
}

func (f *fakeUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var req graphQLRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("要求を JSON として読めない: %v", err)
		}
		f.mu.Lock()
		f.authHeader = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "markPullRequestReadyForReview") {
			f.mu.Lock()
			f.mutations++
			f.mu.Unlock()
			resp := `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_1","isDraft":false}}}}`
			if f.mutationResp != nil {
				resp = f.mutationResp()
			}
			io.WriteString(w, resp)
			return
		}
		resp := `{"data":{"repository":{"pullRequest":{"id":"PR_1","isDraft":true,"headRefOid":"deadbeef","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}}}}`
		if f.pullRequestResp != nil {
			resp = f.pullRequestResp()
		}
		io.WriteString(w, resp)
	}))
}

func newTestClient(url string) *Client {
	return &Client{BaseURL: url, Token: credential.New("test-token")}
}

func TestReadySucceedsWhenChecksGreen(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server(t)
	defer srv.Close()
	c := newTestClient(srv.URL)

	alreadyReady, err := c.Ready(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if alreadyReady {
		t.Fatal("alreadyReady が true (draft のはず)")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mutations != 1 {
		t.Fatalf("mutation の呼び出し回数 = %d, want 1", f.mutations)
	}
	if f.authHeader != "bearer test-token" {
		t.Fatalf("Authorization = %q", f.authHeader)
	}
}

func TestReadyNoopWhenAlreadyNotDraft(t *testing.T) {
	f := &fakeUpstream{
		pullRequestResp: func() string {
			return `{"data":{"repository":{"pullRequest":{"id":"PR_1","isDraft":false,"headRefOid":"deadbeef","commits":{"nodes":[]}}}}}`
		},
	}
	srv := f.server(t)
	defer srv.Close()
	c := newTestClient(srv.URL)

	alreadyReady, err := c.Ready(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !alreadyReady {
		t.Fatal("alreadyReady が false")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mutations != 0 {
		t.Fatalf("draft でないのに mutation が呼ばれた: %d 回", f.mutations)
	}
}

func TestReadyProceedsWhenNoRollup(t *testing.T) {
	f := &fakeUpstream{
		pullRequestResp: func() string {
			return `{"data":{"repository":{"pullRequest":{"id":"PR_1","isDraft":true,"headRefOid":"deadbeef","commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}}`
		},
	}
	srv := f.server(t)
	defer srv.Close()
	c := newTestClient(srv.URL)

	if _, err := c.Ready(context.Background(), "o", "r", 1); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mutations != 1 {
		t.Fatalf("checks が無いのに mutation が呼ばれなかった: %d 回", f.mutations)
	}
}

func TestReadyRefusesWhenChecksNotGreen(t *testing.T) {
	for _, state := range []string{"PENDING", "FAILURE", "ERROR", "EXPECTED"} {
		t.Run(state, func(t *testing.T) {
			f := &fakeUpstream{
				pullRequestResp: func() string {
					return `{"data":{"repository":{"pullRequest":{"id":"PR_1","isDraft":true,"headRefOid":"deadbeef","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"` + state + `"}}}]}}}}}`
				},
			}
			srv := f.server(t)
			defer srv.Close()
			c := newTestClient(srv.URL)

			_, err := c.Ready(context.Background(), "o", "r", 1)
			if !errors.Is(err, ErrChecksNotGreen) {
				t.Fatalf("err = %v, want ErrChecksNotGreen", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.mutations != 0 {
				t.Fatalf("checks が %s なのに mutation が呼ばれた", state)
			}
		})
	}
}

func TestReadyPullRequestNotFound(t *testing.T) {
	f := &fakeUpstream{
		pullRequestResp: func() string { return `{"data":{"repository":{"pullRequest":null}}}` },
	}
	srv := f.server(t)
	defer srv.Close()
	c := newTestClient(srv.URL)

	if _, err := c.Ready(context.Background(), "o", "r", 999); err == nil {
		t.Fatal("PR が無いのに error にならなかった")
	}
}

func TestReadySurfacesGraphQLErrors(t *testing.T) {
	f := &fakeUpstream{
		pullRequestResp: func() string { return `{"data":null,"errors":[{"message":"Could not resolve to a PullRequest"}]}` },
	}
	srv := f.server(t)
	defer srv.Close()
	c := newTestClient(srv.URL)

	_, err := c.Ready(context.Background(), "o", "r", 1)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	if _, err := c.Ready(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("401 なのに error にならなかった")
	}
}

func TestReadyRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxResponseBytes+1))
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	_, err := c.Ready(context.Background(), "o", "r", 1)
	if err == nil || !strings.Contains(err.Error(), "大きすぎる") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyRejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not json")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	if _, err := c.Ready(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("壊れた JSON なのに error にならなかった")
	}
}

// TestReadyMutationErrorPropagates は、mutation 自体が GraphQL error を返したとき、error が伝わることを確かめる
// (mutations のカウンタでなく、戻り値の error で判定する)。
func TestReadyMutationErrorPropagates(t *testing.T) {
	f := &fakeUpstream{
		mutationResp: func() string {
			return `{"data":null,"errors":[{"message":"Resource not accessible by integration"}]}`
		},
	}
	srv := f.server(t)
	defer srv.Close()
	c := newTestClient(srv.URL)

	_, err := c.Ready(context.Background(), "o", "r", 1)
	if err == nil || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("err = %v", err)
	}
}

// TestClientDoUsesGivenNumber は、number の変数が、そのまま query の variables に渡ることを確かめる
// (文字列化や桁落ちが無いこと)。
func TestClientDoUsesGivenNumber(t *testing.T) {
	var gotNumber json.Number
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				Number json.Number `json:"number"`
			} `json:"variables"`
		}
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.UseNumber()
		if err := dec.Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotNumber = req.Variables.Number
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	const want = 4242
	c.Ready(context.Background(), "o", "r", want)
	if gotNumber.String() != strconv.Itoa(want) {
		t.Fatalf("number = %s, want %d", gotNumber, want)
	}
}

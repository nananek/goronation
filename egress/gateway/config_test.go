package gateway

import (
	"testing"

	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

func validPush(t testing.TB) git.Policy {
	t.Helper()
	p, err := git.NewPolicy(git.Repo{Owner: "o", Name: "r"}, testSession)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewAccepts(t *testing.T) {
	push := validPush(t)
	cfg := Config{Push: push, Pull: github.PullPolicy{Push: push}, Credentials: &staticSource{}}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if h.gitBase != defaultGitBaseURL || h.apiBase != defaultAPIBaseURL {
		t.Errorf("base URL の既定値が違う: %s %s", h.gitBase, h.apiBase)
	}
	if h.maxPack != DefaultMaxPackBytes {
		t.Errorf("MaxPackBytes の既定値が違う: %d", h.maxPack)
	}
	// 明示した base URL・上限が使われる。
	cfg.GitBaseURL, cfg.APIBaseURL = "https://git.example.invalid", "https://api.example.invalid"
	cfg.MaxPackBytes, cfg.PRQuota = 123, 7
	h, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if h.gitBase != cfg.GitBaseURL || h.apiBase != cfg.APIBaseURL || h.maxPack != 123 {
		t.Errorf("設定した値が使われない: %+v", h)
	}
}

func TestNewRejectsLiteralPolicy(t *testing.T) {
	// git.NewPolicy を経ない、構造体リテラルの Policy (攻撃者視点レビュー L2 の再現) は、Handler が断る。
	literal := git.Policy{Repo: git.Repo{Owner: "o", Name: "r"}, Session: "a/../../.."}
	if _, err := New(Config{Push: literal, Pull: github.PullPolicy{Push: validPush(t)}, Credentials: &staticSource{}}); err == nil {
		t.Fatal("断るはず")
	}
	if _, err := New(Config{Push: validPush(t), Pull: github.PullPolicy{Push: literal}, Credentials: &staticSource{}}); err == nil {
		t.Fatal("Pull.Push も断るはず")
	}
	// Session が空の構造体リテラルも同様。
	empty := git.Policy{Repo: git.Repo{Owner: "o", Name: "r"}}
	if _, err := New(Config{Push: empty, Pull: github.PullPolicy{Push: validPush(t)}, Credentials: &staticSource{}}); err == nil {
		t.Fatal("空の Session を断るはず")
	}
}

func TestNewRejectsBadTargets(t *testing.T) {
	push := validPush(t)
	bad := github.PullPolicy{Push: push, Targets: []git.Repo{{Owner: "o", Name: "r.git"}}}
	if _, err := New(Config{Push: push, Pull: bad, Credentials: &staticSource{}}); err == nil {
		t.Fatal("Targets の形が正しくない値を断るはず")
	}
}

func TestNewRejectsMissingCredentials(t *testing.T) {
	push := validPush(t)
	if _, err := New(Config{Push: push, Pull: github.PullPolicy{Push: push}}); err == nil {
		t.Fatal("Credentials が nil を断るはず")
	}
}

func TestNewRejectsBadBaseURL(t *testing.T) {
	push := validPush(t)
	base := Config{Push: push, Pull: github.PullPolicy{Push: push}, Credentials: &staticSource{}}
	for _, u := range []string{"not a url", "github.com", "https://", "ftp://x", "https://x/path", "https://x?q=1", "https://x#f"} {
		cfg := base
		cfg.GitBaseURL = u
		if _, err := New(cfg); err == nil {
			t.Errorf("GitBaseURL %q を断るはず", u)
		}
		cfg = base
		cfg.APIBaseURL = u
		if _, err := New(cfg); err == nil {
			t.Errorf("APIBaseURL %q を断るはず", u)
		}
	}
}

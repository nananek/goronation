package github

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nananek/goronation/egress/git"
)

const base = PathPrefix + "repos/"

func TestParsePullRoute(t *testing.T) {
	for _, c := range []struct {
		target string
		want   git.Repo
		path   string
	}{
		{base + "o/r/pulls", git.Repo{Owner: "o", Name: "r"}, "/repos/o/r/pulls"},
		{base + "My-Org/.GitHub/pulls", git.Repo{Owner: "My-Org", Name: ".GitHub"}, "/repos/my-org/.github/pulls"},
		{base + "a/b.c_d-e/pulls", git.Repo{Owner: "a", Name: "b.c_d-e"}, "/repos/a/b.c_d-e/pulls"},
	} {
		got, err := ParsePullRoute("POST", c.target)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v, %v", c.target, got, err)
			continue
		}
		if PullsPath(got) != c.path || RepoPath(got) != strings.TrimSuffix(c.path, "/pulls") {
			t.Errorf("上流の path = %q %q", PullsPath(got), RepoPath(got))
		}
	}
}

func TestParsePullRouteRejects(t *testing.T) {
	cases := []struct{ method, target string }{
		// method
		{"GET", base + "o/r/pulls"},
		{"PUT", base + "o/r/pulls"},
		{"PATCH", base + "o/r/pulls"},
		{"DELETE", base + "o/r/pulls"},
		{"HEAD", base + "o/r/pulls"},
		{"post", base + "o/r/pulls"},
		{"", base + "o/r/pulls"},
		// PR の作成以外の API (undraft・更新・マージ・issue・repo の情報)
		{"POST", base + "o/r/pulls/1"},
		{"POST", base + "o/r/pulls/1/merge"},
		{"POST", base + "o/r/pulls/1/ready_for_review"},
		{"PATCH", base + "o/r/pulls/1"},
		{"POST", base + "o/r/issues"},
		{"POST", base + "o/r/issues/1/comments"},
		{"POST", base + "o/r/git/refs"},
		{"POST", base + "o/r/statuses/abc"},
		{"POST", base + "o/r"},
		{"GET", base + "o/r"},
		{"POST", PathPrefix + "graphql"},
		{"POST", PathPrefix + "repos/o/r/pulls/"},
		{"POST", PathPrefix + "user"},
		{"POST", PathPrefix + "repos"},
		{"POST", PathPrefix},
		{"POST", "/github-api"},
		// path の形
		{"POST", base + "o/r/pulls/"},
		{"POST", base + "o/r/Pulls"},
		{"POST", base + "o/r/pulls/../pulls"},
		{"POST", base + "o//pulls"},
		{"POST", base + "/r/pulls"},
		{"POST", base + "o/r//pulls"},
		{"POST", base + "../r/pulls"},
		{"POST", base + "o/../pulls"},
		{"POST", base + "o/../../repos/x/y/pulls"},
		{"POST", base + "o/./pulls"},
		{"POST", base + "o/r.git/pulls"},
		{"POST", base + "-o/r/pulls"},
		{"POST", base + strings.Repeat("a", 40) + "/r/pulls"},
		{"POST", base + "o/" + strings.Repeat("a", 101) + "/pulls"},
		{"POST", "o/r/pulls"},
		{"POST", "repos/o/r/pulls"},
		{"POST", "/api/repos/o/r/pulls"},
		{"POST", "/GITHUB-API/repos/o/r/pulls"},
		{"POST", "/github-apix/repos/o/r/pulls"},
		{"POST", "http://127.0.0.1:3128" + base + "o/r/pulls"},
		{"POST", "//api.github.com" + base + "o/r/pulls"},
		{"POST", "*"},
		{"POST", ""},
		// query・文字
		{"POST", base + "o/r/pulls?"},
		{"POST", base + "o/r/pulls?draft=false"},
		{"POST", base + "o/r/pulls#x"},
		{"POST", base + "o/r/%70ulls"},
		{"POST", base + "o%2fx/r/pulls"},
		{"POST", base + "%2e%2e/r/pulls"},
		{"POST", base + `o\x/r/pulls`},
		{"POST", base + "o/r/pulls "},
		{"POST", base + "o/r/pulls\n"},
		{"POST", base + "o/r/pulls\x00"},
		{"POST", base + "o/r/pulls\x7f"},
		{"POST", base + "o/r/pulls\x1b[31m"},
		{"POST", base + "日本/r/pulls"},
		{"POST", base + "o/r/pulls\xff"},
	}
	for _, c := range cases {
		got, err := ParsePullRoute(c.method, c.target)
		if err == nil {
			t.Errorf("断るはず: %s %q → %+v", c.method, c.target, got)
			continue
		}
		if Reason(err) != CodeBadRoute || !safeText(err.Error()) || len(err.Error()) > 400 {
			t.Errorf("%s %q: Code = %q, err = %v", c.method, c.target, Reason(err), err)
		}
	}
}

var pullsShape = regexp.MustCompile(`^/repos/[a-z0-9-]{1,39}/[a-z0-9._-]{1,100}/pulls$`)

// FuzzParsePullRoute は、通した経路の上流の path が、固定の形で、要求の path と (小文字にして) 一致することを確かめる。
func FuzzParsePullRoute(f *testing.F) {
	for _, s := range []string{base + "o/r/pulls", base + "o/r/pulls/1", base + "o/../pulls", base + "%2e", "", "/", PathPrefix} {
		f.Add("POST", s)
		f.Add("GET", s)
	}
	f.Fuzz(func(t *testing.T, method, target string) {
		repo, err := ParsePullRoute(method, target)
		if err != nil {
			if Reason(err) != CodeBadRoute || !safeText(err.Error()) {
				t.Fatalf("エラーが安全でない: %v", err)
			}
			return
		}
		if method != "POST" {
			t.Fatalf("POST 以外を通した: %q", method)
		}
		p := PullsPath(repo)
		if !pullsShape.MatchString(p) || !strings.EqualFold(PathPrefix+p[1:], target) {
			t.Fatalf("上流の path %q が、要求 %q と合わない", p, target)
		}
	})
}

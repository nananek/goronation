package git

import (
	"regexp"
	"strings"
	"testing"
)

const pre = PathPrefix

func TestParseRouteAccepts(t *testing.T) {
	cases := []struct {
		method, target string
		want           Route
		upstream       string
	}{
		{"GET", pre + "o/r.git/info/refs?service=git-upload-pack", Route{Repo{"o", "r"}, OpAdvertiseUpload}, "/o/r.git/info/refs?service=git-upload-pack"},
		{"GET", pre + "o/r.git/info/refs?service=git-receive-pack", Route{Repo{"o", "r"}, OpAdvertiseReceive}, "/o/r.git/info/refs?service=git-receive-pack"},
		{"POST", pre + "o/r.git/git-upload-pack", Route{Repo{"o", "r"}, OpUploadPack}, "/o/r.git/git-upload-pack"},
		{"POST", pre + "o/r.git/git-receive-pack", Route{Repo{"o", "r"}, OpReceivePack}, "/o/r.git/git-receive-pack"},
		// owner と name は、小文字にして上流へ送る。
		{"POST", pre + "My-Org/.GitHub.git/git-receive-pack", Route{Repo{"My-Org", ".GitHub"}, OpReceivePack}, "/my-org/.github.git/git-receive-pack"},
		{"GET", pre + "a/b.c_d-e.git/info/refs?service=git-upload-pack", Route{Repo{"a", "b.c_d-e"}, OpAdvertiseUpload}, "/a/b.c_d-e.git/info/refs?service=git-upload-pack"},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.method, c.target)
		if err != nil || got != c.want {
			t.Errorf("%s %s: %+v, %v", c.method, c.target, got, err)
			continue
		}
		if got.Upstream() != c.upstream {
			t.Errorf("Upstream = %q, 期待 %q", got.Upstream(), c.upstream)
		}
	}
}

func TestParseRouteRejects(t *testing.T) {
	long := strings.Repeat("a", 101)
	cases := []struct{ method, target string }{
		// method の取り違え
		{"POST", pre + "o/r.git/info/refs?service=git-upload-pack"},
		{"GET", pre + "o/r.git/git-upload-pack"},
		{"GET", pre + "o/r.git/git-receive-pack"},
		{"PUT", pre + "o/r.git/git-receive-pack"},
		{"DELETE", pre + "o/r.git/git-receive-pack"},
		{"HEAD", pre + "o/r.git/info/refs?service=git-upload-pack"},
		{"CONNECT", pre + "o/r.git/git-receive-pack"},
		{"get", pre + "o/r.git/info/refs?service=git-upload-pack"},
		{"post", pre + "o/r.git/git-receive-pack"},
		{"", pre + "o/r.git/git-receive-pack"},
		// query
		{"GET", pre + "o/r.git/info/refs"},
		{"GET", pre + "o/r.git/info/refs?"},
		{"GET", pre + "o/r.git/info/refs?service="},
		{"GET", pre + "o/r.git/info/refs?service=git-upload-archive"},
		{"GET", pre + "o/r.git/info/refs?service=git-upload-pack&x=1"},
		{"GET", pre + "o/r.git/info/refs?x=1&service=git-upload-pack"},
		{"GET", pre + "o/r.git/info/refs?Service=git-upload-pack"},
		{"GET", pre + "o/r.git/info/refs?service=Git-Upload-Pack"},
		{"GET", pre + "o/r.git/info/refs?service=git-upload-pack&service=git-receive-pack"},
		{"GET", pre + "o/r.git/info/refs?service=git-upload-pack%00"},
		{"POST", pre + "o/r.git/git-receive-pack?"},
		{"POST", pre + "o/r.git/git-receive-pack?service=git-receive-pack"},
		// path の形
		{"GET", "/o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "/git/o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "/git/github.com"},
		{"GET", "/git/github.com/"},
		{"GET", "/git/github.com/o"},
		{"GET", "/git/github.com/o/"},
		{"GET", "/git/github.com/o/r"},
		{"GET", pre + "o/r.git"},
		{"GET", pre + "o/r.git/"},
		{"POST", pre + "o/r.git/git-receive-pack/"},
		{"POST", pre + "o/r.git//git-receive-pack"},
		{"GET", pre + "o/r.git/info/refs/"},
		{"GET", pre + "o/r.git/info//refs?service=git-upload-pack"},
		{"GET", pre + "o/r.git/HEAD"},
		{"GET", pre + "o/r.git/objects/info/packs"},
		{"GET", pre + "o/r.git/info/refs/x?service=git-upload-pack"},
		{"POST", pre + "o/r.git/git-upload-archive"},
		{"POST", pre + "o/r.git/Git-Receive-Pack"},
		{"POST", pre + "o/r.git/git-receive-pack/x"},
		{"POST", pre + "o/r/git-receive-pack"},
		{"POST", pre + "o/r.git.git/git-receive-pack"},
		{"POST", pre + "o/r.GIT.git/git-receive-pack"},
		{"POST", pre + "o/.git/git-receive-pack"},
		{"POST", pre + "o/..git/git-receive-pack"},
		{"POST", pre + "o/...git/git-receive-pack"},
		{"POST", pre + "//r.git/git-receive-pack"},
		{"POST", pre + "o//r.git/git-receive-pack"},
		{"POST", pre + "../r.git/git-receive-pack"},
		{"POST", pre + "./r.git/git-receive-pack"},
		{"POST", pre + "o/../r.git/git-receive-pack"},
		{"POST", pre + "o/../../r.git/git-receive-pack"},
		{"POST", pre + "-o/r.git/git-receive-pack"},
		{"POST", pre + "o-/r.git/git-receive-pack"},
		{"POST", pre + strings.Repeat("a", 40) + "/r.git/git-receive-pack"},
		{"POST", pre + "o/" + long + ".git/git-receive-pack"},
		{"POST", pre + "x/o/r.git/git-receive-pack"},
		// 接頭辞
		{"GET", "/GIT/github.com/o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "/git/GitHub.com/o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "/git/github.com.evil/o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "/git/github.com:443/o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "http://127.0.0.1:3128" + pre + "o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "//github.com" + pre + "o/r.git/info/refs?service=git-upload-pack"},
		{"GET", "*"},
		{"GET", ""},
		// 文字
		{"POST", pre + "o/r%2egit/git-receive-pack"},
		{"POST", pre + "o/r.git/git-receive-pack%2f"},
		{"POST", pre + "o%2fx/r.git/git-receive-pack"},
		{"POST", pre + "%2e%2e/r.git/git-receive-pack"},
		{"POST", pre + "o/r.git/git-receive-pack#frag"},
		{"POST", pre + `o\x/r.git/git-receive-pack`},
		{"POST", pre + "o/r.git/git-receive-pack "},
		{"POST", pre + "o/r .git/git-receive-pack"},
		{"POST", pre + "o/r\t.git/git-receive-pack"},
		{"POST", pre + "o/r.git/git-receive-pack\n"},
		{"POST", pre + "o/r.git/git-receive-pack\r\n"},
		{"POST", pre + "o/r.git/git-receive-pack\x00"},
		{"POST", pre + "o/r.git/git-receive-pack\x7f"},
		{"POST", pre + "o/r.git/git-receive-pack\x1b[31m"},
		{"POST", pre + "日本/r.git/git-receive-pack"},
		{"POST", pre + "o/日本.git/git-receive-pack"},
		{"POST", pre + "o/r.git/git-receive-pack\xff"},
		{"GET", pre + "o/r.git/info/refs?service=git-upload-pack#x"},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.method, c.target)
		if err == nil {
			t.Errorf("断るはず: %s %q → %+v", c.method, c.target, got)
			continue
		}
		if Reason(err) != CodeBadRoute || !safeText(err.Error()) || len(err.Error()) > 400 {
			t.Errorf("%s %q: Code = %q, err = %v", c.method, c.target, Reason(err), err)
		}
	}
}

func TestOp(t *testing.T) {
	for op, w := range map[Op]struct {
		name, req string
		writes    bool
	}{
		OpAdvertiseUpload:  {"advertise-upload-pack", "", false},
		OpAdvertiseReceive: {"advertise-receive-pack", "", true},
		OpUploadPack:       {"upload-pack", "application/x-git-upload-pack-request", false},
		OpReceivePack:      {"receive-pack", "application/x-git-receive-pack-request", true},
	} {
		if op.String() != w.name || op.RequestType() != w.req || op.Writes() != w.writes {
			t.Errorf("%d: %q %q %v", op, op.String(), op.RequestType(), op.Writes())
		}
	}
	var none Op
	if none.String() != "unknown" || none.Writes() || none.RequestType() != "" || (Route{}).Upstream() != "" {
		t.Errorf("Op の 0 値は、何も許さない")
	}
}

var upstreamShape = regexp.MustCompile(`^/[a-z0-9-]{1,39}/[a-z0-9._-]{1,100}\.git/(info/refs\?service=git-(upload|receive)-pack|git-(upload|receive)-pack)$`)

// FuzzParseRoute は、通した経路の上流の path が、固定の 4 つの形のどれかで、要求の文字が (owner・name 以外は) 混ざらないことを確かめる。
func FuzzParseRoute(f *testing.F) {
	for _, s := range []string{pre + "o/r.git/git-receive-pack", pre + "o/r.git/info/refs?service=git-upload-pack", pre + "o/../r.git/x", pre + "%2e", "", "/", pre} {
		f.Add("POST", s)
		f.Add("GET", s)
	}
	f.Fuzz(func(t *testing.T, method, target string) {
		route, err := ParseRoute(method, target)
		if err != nil {
			if Reason(err) != CodeBadRoute || !safeText(err.Error()) {
				t.Fatalf("エラーが安全でない: %v", err)
			}
			return
		}
		if u := route.Upstream(); !upstreamShape.MatchString(u) {
			t.Fatalf("上流の path が固定の形ではない: %q (要求 %q)", u, target)
		}
		// 通した要求を、元の綴りのまま作り直せる (canonical)。
		want := pre + route.Repo.String() + ".git/"
		if !strings.HasPrefix(target, want) {
			t.Fatalf("要求 %q が、検査した repo %v から作れない", target, route.Repo)
		}
		if route.Op.Writes() != (strings.Contains(target, "receive-pack")) {
			t.Fatalf("Op の判定が、要求と食い違う: %v %q", route.Op, target)
		}
	})
}

package git

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func newPolicy(t testing.TB) Policy {
	t.Helper()
	p, err := NewPolicy(Repo{"o", "r"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckRef(t *testing.T) {
	p := newPolicy(t)
	ok := []string{
		pfx + "x", pfx + "feat/x/y", pfx + "a.b", pfx + "a_b-c", pfx + "v1.2.3", pfx + "-x", pfx + "A",
		pfx + strings.Repeat("a", 200-len(pfx)),
	}
	for _, ref := range ok {
		if err := p.CheckRef(ref); err != nil {
			t.Errorf("通るはず: %q: %v", ref, err)
		}
	}
	bad := []string{
		"", "refs/heads/main", "refs/heads/develop", "refs/tags/t1", "refs/tags/goronation/" + sess + "/x", "refs/heads/goronation/other/x",
		"refs/heads/goronation/" + sess, "refs/heads/goronation/" + sess + "x/y", "refs/heads/goronation/" + sess[:len(sess)-1] + "/x",
		"refs/heads/goronation//x", "refs/remotes/origin/x", "refs/for/main", "HEAD", "refs/heads/GORONATION/" + sess + "/x", "REFS/heads/goronation/" + sess + "/x",
		"refs/heads/goronation/" + sess + "/../../main", "refs/heads/../goronation/" + sess + "/x", " " + pfx + "x", pfx + "x ",
		pfx, pfx + "/x", pfx + "x/", pfx + "a//b", pfx + ".x", pfx + "x.", pfx + "a..b", pfx + "a/../b", pfx + "a/./b", pfx + "x.lock", pfx + "a.lock/b",
		pfx + "a b", pfx + "a:b", pfx + "a~b", pfx + "a^b", pfx + "a?b", pfx + "a*b", pfx + "a[b", pfx + `a\b`, pfx + "a@{b", pfx + "@", pfx + "a%2fb",
		pfx + "a\nb", pfx + "a\x00b", pfx + "a\x7fb", pfx + "\x1b[31m", pfx + "日本語", pfx + "a\xe2\x80\x8bb",
		pfx + strings.Repeat("a", 201-len(pfx)), pfx + strings.Repeat("a", 5000),
	}
	for _, ref := range bad {
		err := p.CheckRef(ref)
		if err == nil {
			t.Errorf("断るはず: %q", ref)
			continue
		}
		if Reason(err) != CodeRefNotAllowed || !safeText(err.Error()) {
			t.Errorf("%q: Code = %q, err = %v", ref, Reason(err), err)
		}
	}
}

func TestNewPolicy(t *testing.T) {
	r := Repo{"o", "r"}
	for _, s := range []string{"a", "20260927-041500-a1b2c3", "A.b_c-d", strings.Repeat("a", 64)} {
		if _, err := NewPolicy(r, s); err != nil {
			t.Errorf("セッション %q は通るはず: %v", s, err)
		}
	}
	for _, s := range []string{"", "a/b", "/a", "a/", ".a", "a.", "a..b", "a.lock", "a b", "a:b", "a\nb", "日本", strings.Repeat("a", 65), "..", "@", "a*"} {
		if _, err := NewPolicy(r, s); Reason(err) != CodeRefNotAllowed {
			t.Errorf("セッション %q は断るはず: %v", s, err)
		}
	}
	for _, bad := range []Repo{{"", "r"}, {"o", ""}, {"o/x", "r"}, {"o", "r/x"}, {"-o", "r"}, {"o", ".."}, {"o", "r.git"}} {
		if _, err := NewPolicy(bad, sess); Reason(err) != CodeBadRoute {
			t.Errorf("repo %+v は断るはず: %v", bad, err)
		}
	}
}

func TestCheckRepo(t *testing.T) {
	p, _ := NewPolicy(Repo{"Nananek", "Goronation"}, sess)
	for _, r := range []Repo{{"Nananek", "Goronation"}, {"nananek", "goronation"}, {"NANANEK", "GORONATION"}} {
		if err := p.CheckRepo(r); err != nil {
			t.Errorf("%v は通るはず: %v", r, err)
		}
	}
	for _, r := range []Repo{{"nananek", "goronation2"}, {"nananek2", "goronation"}, {"nananek", "goro"}, {"x", "goronation"}, {"goronation", "nananek"}, {"", ""}} {
		if err := p.CheckRepo(r); Reason(err) != CodeRepoNotAllowed {
			t.Errorf("%v は断るはず: %v", r, err)
		}
	}
}

func TestParseRepo(t *testing.T) {
	for s, want := range map[string]Repo{"o/r": {"o", "r"}, "My-Org/.github": {"My-Org", ".github"}, "a/b.c_d-e": {"a", "b.c_d-e"}} {
		got, err := ParseRepo(s)
		if err != nil || got != want || got.String() != s {
			t.Errorf("%q: %v %v", s, got, err)
		}
	}
	for _, s := range []string{"", "o", "/r", "o/", "o/r/x", "o/r.git", "o/R.GIT", "o/..", "o/.", "-o/r", "o-/r", "o o/r", "o/r r", strings.Repeat("a", 40) + "/r", "o/" + strings.Repeat("a", 101), "日本/r", "o/r\n"} {
		if _, err := ParseRepo(s); Reason(err) != CodeBadRoute {
			t.Errorf("%q は断るはず: %v", s, err)
		}
	}
}

func TestCloneURL(t *testing.T) {
	for s, want := range map[string]string{
		"o/r":            "https://github.com/o/r.git",
		"My-Org/.github": "https://github.com/My-Org/.github.git",
		"a/b.c_d-e":      "https://github.com/a/b.c_d-e.git",
	} {
		repo, err := ParseRepo(s)
		if err != nil {
			t.Fatalf("%q: ParseRepo: %v", s, err)
		}
		if got := repo.CloneURL(); got != want {
			t.Errorf("CloneURL(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestCheck(t *testing.T) {
	p := newPolicy(t)
	c := func(old, new, ref string) Command { return Command{old, new, ref} }
	cases := []struct {
		name string
		cmds []Command
		code string
	}{
		{"probe", nil, ""},
		{"create", cmds(c(zero(), oid1, pfx+"x")), ""},
		{"update", cmds(c(oid1, oid2, pfx+"x")), ""},
		{"many", cmds(c(zero(), oid1, pfx+"a"), c(oid1, oid2, pfx+"b/c")), ""},
		{"delete", cmds(c(oid1, zero(), pfx+"x")), CodeDelete},
		{"delete-outside", cmds(c(oid1, zero(), "refs/heads/main")), CodeDelete},
		{"both-zero", cmds(c(zero(), zero(), pfx+"x")), CodeDelete},
		{"main", cmds(c(oid1, oid2, "refs/heads/main")), CodeRefNotAllowed},
		{"tag", cmds(c(zero(), oid1, "refs/tags/t")), CodeRefNotAllowed},
		{"one-bad-of-many", cmds(c(zero(), oid1, pfx+"a"), c(zero(), oid1, "refs/heads/main"), c(zero(), oid1, pfx+"b")), CodeRefNotAllowed},
		{"one-delete-of-many", cmds(c(zero(), oid1, pfx+"a"), c(oid1, zero(), pfx+"b")), CodeDelete},
	}
	for _, tc := range cases {
		err := p.Check(&Push{Commands: tc.cmds})
		if Reason(err) != tc.code || (tc.code == "") != (err == nil) {
			t.Errorf("%s: err = %v, 期待 %q", tc.name, err, tc.code)
		}
	}
}

// TestReadPushCaptured は、実物の本文に、ParseCommands と Check を続けて通した結果 (どれを通し、どれを断るか) を固定する。
func TestReadPushCaptured(t *testing.T) {
	want := map[string]string{
		"create": "", "update": "", "progress": "", "multi": "", "atomic": "", "copy": "", "force": "", "nested": "", "big": "", "big-probe": "",
		"delete": CodeDelete, "tag": CodeRefNotAllowed, "lighttag": CodeRefNotAllowed, "main": CodeRefNotAllowed, "mixed": CodeRefNotAllowed,
		"pushopt": CodePushOptions, "shallow": CodeShallow, "signed": CodePushCert, "unicode": CodeBadCommand, "sha256": CodeBadCommand,
	}
	p := newPolicy(t)
	other, _ := NewPolicy(Repo{"o", "r"}, "20260927-041500-ffffff")
	files, _ := filepath.Glob(filepath.Join(capturedD, "*.bin"))
	if len(files) != len(want) {
		t.Fatalf("採取したファイルは %d 個のはず", len(want))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".bin")
		data, _ := os.ReadFile(f)
		push, err := p.ReadPush(bytes.NewReader(data), Limits{})
		if Reason(err) != want[name] || (want[name] == "") != (err == nil) {
			t.Errorf("%s: err = %v, 期待 %q", name, err, want[name])
		}
		if err != nil && push != nil {
			t.Errorf("%s: 断ったのに Push を返した", name)
		}
		// 別のセッションの名前空間では、コマンドのあるものは、全て断られる (他のセッションの ref に触れない)。
		_, err = other.ReadPush(bytes.NewReader(data), Limits{})
		if want[name] == "" && name != "big-probe" && Reason(err) != CodeRefNotAllowed {
			t.Errorf("%s: 別のセッションでは断るはず: %v", name, err)
		}
	}
}

func TestCheckBranchName(t *testing.T) {
	for _, s := range []string{"main", "develop", "release/1.0", "feature-x", "v1.2", "a/b/c", "A_b", strings.Repeat("a", 200)} {
		if err := CheckBranchName(s); err != nil {
			t.Errorf("%q は通るはず: %v", s, err)
		}
	}
	for _, s := range []string{"", "-x", "a//b", "/a", "a/", ".x", "x.", "x.lock", "a..b", "a b", "a:b", "a\nb", "日本", "a~1", "a^", "a@{1}", strings.Repeat("a", 201), "a/../b"} {
		if err := CheckBranchName(s); Reason(err) != CodeRefNotAllowed {
			t.Errorf("%q は断るはず: %v", s, err)
		}
	}
}

// TestCheckRefMatchesModel は、CheckRef と、別の書き方 (正規表現) の判定が、任意の名前で一致することを、fuzz の種だけで確かめる。
var refModel = regexp.MustCompile(`^refs/heads/goronation/` + regexp.QuoteMeta(sess) + `/([A-Za-z0-9_-][A-Za-z0-9._-]*[A-Za-z0-9_-]|[A-Za-z0-9_-])(/([A-Za-z0-9_-][A-Za-z0-9._-]*[A-Za-z0-9_-]|[A-Za-z0-9_-]))*$`)

// FuzzCheckRef は、CheckRef が通した ref が、独立した規則 (正規表現・長さ・.. と .lock の不在) を満たし、逆も成り立つことを確かめる。
func FuzzCheckRef(f *testing.F) {
	for _, s := range []string{pfx + "x", pfx + "a/b", pfx, pfx + ".x", pfx + "a..b", pfx + "a.lock", "refs/heads/main", pfx + "日", pfx + "a\x00"} {
		f.Add(s)
	}
	p, _ := NewPolicy(Repo{"o", "r"}, sess)
	f.Fuzz(func(t *testing.T, ref string) {
		err := p.CheckRef(ref)
		model := refModel.MatchString(ref) && len(ref) <= 200 && !strings.Contains(ref, "..") && !strings.Contains(ref, ".lock/") && !strings.HasSuffix(ref, ".lock")
		if (err == nil) != model {
			t.Fatalf("CheckRef(%q) = %v、独立の規則 = %v", ref, err, model)
		}
		if err != nil && (Reason(err) != CodeRefNotAllowed || !safeText(err.Error())) {
			t.Fatalf("エラーが安全でない: %v", err)
		}
	})
}

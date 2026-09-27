package github

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/nananek/goronation/egress/git"
)

const sess = "20260927-041500-a1b2c3"

var (
	repoOR   = git.Repo{Owner: "o", Name: "r"}
	repoUp   = git.Repo{Owner: "up", Name: "repo"}
	repoFork = git.Repo{Owner: "me", Name: "repo"}
)

func policyFor(t testing.TB, repo git.Repo, targets ...git.Repo) PullPolicy {
	t.Helper()
	p, err := git.NewPolicy(repo, sess)
	if err != nil {
		t.Fatal(err)
	}
	return PullPolicy{Push: p, Targets: targets}
}

// safeText は、端末に出してよい文字列か (git の同名のテスト補助と同じ規則)。
func safeText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == ' ' || r > 0x20 && r < 0x7f:
		case r < 0x20 || r >= 0x7f && r < 0x100:
			return false
		case unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han), r == 0x30fb, r == 0x30fc, r >= 0x3001 && r <= 0x303f, r >= 0xff01 && r <= 0xff5e:
		default:
			return false
		}
	}
	return true
}

func headOf(branch string) string { return "goro/" + sess + "/" + branch }

// pullBody は、要求の JSON を作る (項目の値は json.Marshal で書く)。
func pullBody(t testing.TB, kv ...any) string {
	t.Helper()
	var parts []string
	for i := 0; i < len(kv); i += 2 {
		k, _ := json.Marshal(kv[i])
		v, err := json.Marshal(kv[i+1])
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, string(k)+":"+string(v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestParsePullAccepts(t *testing.T) {
	same := policyFor(t, repoOR)
	fork := policyFor(t, repoFork, repoUp)
	both := policyFor(t, repoFork, repoFork, repoUp)
	cases := []struct {
		name     string
		pp       PullPolicy
		repo     git.Repo
		body     string
		wantJSON string // 上流に送る本文 (base が空なら WithBase("main") した後)
		wantBase string
	}{
		{"minimal", same, repoOR, `{"title":"t","head":"` + headOf("x") + `","base":"main"}`,
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"body-and-draft", same, repoOR, pullBody(t, "title", "題", "body", "本文\nline2\r\n\ttab", "head", headOf("x"), "base", "develop", "draft", true),
			`{"title":"題","body":"本文\nline2\r\n\ttab","head":"` + headOf("x") + `","base":"develop","draft":true}`, "develop"},
		{"draft-false-is-overridden", same, repoOR, pullBody(t, "title", "t", "head", headOf("x"), "base", "main", "draft", false),
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"base-omitted", same, repoOR, pullBody(t, "title", "t", "head", headOf("x")),
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, ""},
		{"order-and-space", same, repoOR, "\n {\t\"draft\" : true , \"base\":\"main\",\r\n\"head\":\"" + headOf("x") + "\", \"title\":\"t\" }\n ",
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"escapes", same, repoOR, `{"title":"\u0041\"\\\/","head":"` + headOf("x") + `","base":"main"}`,
			`{"title":"A\"\\/","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"html-chars", same, repoOR, pullBody(t, "title", "<a&b>", "head", headOf("x"), "base", "main"),
			`{"title":"\u003ca\u0026b\u003e","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"empty-body-is-omitted", same, repoOR, pullBody(t, "title", "t", "body", "", "head", headOf("x"), "base", "main"),
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"nested-branch", same, repoOR, pullBody(t, "title", "t", "head", headOf("feat/a.b/c"), "base", "release/1.0"),
			`{"title":"t","head":"` + headOf("feat/a.b/c") + `","base":"release/1.0","draft":true}`, "release/1.0"},
		{"same-repo-owner-form-is-canonicalised", same, repoOR, pullBody(t, "title", "t", "head", "O:"+headOf("x"), "base", "main"),
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"case-insensitive-repo", same, git.Repo{Owner: "O", Name: "R"}, pullBody(t, "title", "t", "head", headOf("x"), "base", "main"),
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"fork", fork, repoUp, pullBody(t, "title", "t", "head", "me:"+headOf("x"), "base", "main"),
			`{"title":"t","head":"me:` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"fork-owner-case", fork, repoUp, pullBody(t, "title", "t", "head", "ME:"+headOf("x"), "base", "main"),
			`{"title":"t","head":"me:` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"both-targets-fork-repo", both, repoFork, pullBody(t, "title", "t", "head", headOf("x"), "base", "main"),
			`{"title":"t","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"both-targets-upstream", both, repoUp, pullBody(t, "title", "t", "head", "me:"+headOf("x"), "base", "main"),
			`{"title":"t","head":"me:` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"max-title", same, repoOR, pullBody(t, "title", strings.Repeat("a", MaxTitleBytes), "head", headOf("x"), "base", "main"),
			`{"title":"` + strings.Repeat("a", MaxTitleBytes) + `","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
		{"max-body", same, repoOR, pullBody(t, "title", "t", "body", strings.Repeat("b", MaxBodyBytes), "head", headOf("x"), "base", "main"),
			`{"title":"t","body":"` + strings.Repeat("b", MaxBodyBytes) + `","head":"` + headOf("x") + `","base":"main","draft":true}`, "main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pull, err := c.pp.ParsePull(c.repo, []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if pull.Base != c.wantBase {
				t.Fatalf("Base = %q, 期待 %q", pull.Base, c.wantBase)
			}
			if pull.Base == "" {
				if _, err := pull.JSON(); err == nil {
					t.Fatalf("base が空のまま、JSON にできた")
				}
				if pull, err = pull.WithBase("main"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := pull.JSON()
			if err != nil || string(got) != c.wantJSON {
				t.Fatalf("JSON:\n got  %s\n want %s\n err %v", got, c.wantJSON, err)
			}
			// 許可した綴りの repo が入る (要求の綴りではない)。
			if !pull.Repo.Equal(c.repo) || (c.name == "case-insensitive-repo") != (pull.Repo != c.repo) {
				t.Fatalf("Repo = %+v", pull.Repo)
			}
		})
	}
}

// TestParsePullSizeBoundary は、要求の大きさの上限が、ちょうどの大きさを通し、1 バイト超えを断ることを確かめる。
func TestParsePullSizeBoundary(t *testing.T) {
	pp := policyFor(t, repoOR)
	b := pullBody(t, "title", "t", "head", headOf("x"), "base", "main")
	if _, err := pp.ParsePull(repoOR, []byte(b+strings.Repeat(" ", MaxRequestBytes-len(b)))); err != nil {
		t.Fatalf("ちょうどの大きさは通る: %v", err)
	}
	if _, err := pp.ParsePull(repoOR, []byte(b+strings.Repeat(" ", MaxRequestBytes-len(b)+1))); Reason(err) != CodeTooLarge {
		t.Fatalf("1 バイト超えは断る: %v", err)
	}
}

func TestParsePullRejects(t *testing.T) {
	same := policyFor(t, repoOR)
	fork := policyFor(t, repoFork, repoUp)
	ok := func(kv ...any) string { // 通る本文を土台に、項目を上書きする (無ければ足す)
		fields := []any{"title", "t", "head", headOf("x"), "base", "main"}
		for i := 0; i < len(kv); i += 2 {
			found := false
			for j := 0; j < len(fields); j += 2 {
				if fields[j] == kv[i] {
					fields[j+1], found = kv[i+1], true
				}
			}
			if !found {
				fields = append(fields, kv[i], kv[i+1])
			}
		}
		return pullBody(t, fields...)
	}
	only := func(kv ...any) string { return pullBody(t, kv...) }
	hd := headOf
	type tc struct {
		name string
		pp   PullPolicy
		repo git.Repo
		body string
		code string
	}
	cases := []tc{
		// repo
		{"repo-other", same, git.Repo{Owner: "x", Name: "y"}, ok(), CodeRepoNotAllowed},
		{"repo-similar", same, git.Repo{Owner: "o", Name: "r2"}, ok(), CodeRepoNotAllowed},
		{"fork-push-repo-is-not-a-target", fork, repoFork, ok("head", "me:"+hd("x")), CodeRepoNotAllowed},
		// 大きさ・形式
		{"too-large", same, repoOR, strings.Repeat(" ", MaxRequestBytes+1), CodeTooLarge},
		{"invalid-utf8", same, repoOR, "{\"title\":\"\xff\",\"head\":\"" + hd("x") + "\"}", CodeBadJSON},
		{"empty", same, repoOR, "", CodeBadJSON},
		{"spaces", same, repoOR, "  ", CodeBadJSON},
		{"array", same, repoOR, "[]", CodeBadJSON},
		{"string", same, repoOR, `"x"`, CodeBadJSON},
		{"null", same, repoOR, "null", CodeBadJSON},
		{"number", same, repoOR, "1", CodeBadJSON},
		{"open-only", same, repoOR, "{", CodeBadJSON},
		{"key-only", same, repoOR, `{"title"`, CodeBadJSON},
		{"unterminated", same, repoOR, `{"title":"a"`, CodeBadJSON},
		{"trailing-garbage", same, repoOR, ok() + "x", CodeBadJSON},
		{"two-objects", same, repoOR, ok() + ok(), CodeBadJSON},
		{"object-then-space-then-object", same, repoOR, ok() + " " + ok(), CodeBadJSON},
		{"trailing-comma", same, repoOR, `{"title":"t","head":"` + hd("x") + `",}`, CodeBadJSON},
		{"bom", same, repoOR, "\xef\xbb\xbf" + ok(), CodeBadJSON},
		{"unquoted-key", same, repoOR, `{title:"t"}`, CodeBadJSON},
		{"single-quotes", same, repoOR, `{'title':'t'}`, CodeBadJSON},
		{"comment", same, repoOR, `{"title":"t"/*x*/,"head":"` + hd("x") + `"}`, CodeBadJSON},
		{"deep-value", same, repoOR, `{"title":` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}`, CodeBadJSON},
		// 項目
		{"unknown-maintainer_can_modify", same, repoOR, ok("maintainer_can_modify", true), CodeUnknownField},
		{"unknown-issue", same, repoOR, ok("issue", 1), CodeUnknownField},
		{"unknown-head_repo", same, repoOR, ok("head_repo", "x/y"), CodeUnknownField},
		{"unknown-labels", same, repoOR, ok("labels", []string{"x"}), CodeUnknownField},
		{"unknown-deep", same, repoOR, `{"x":` + strings.Repeat("[", 20000) + `}`, CodeUnknownField},
		{"case-Title", same, repoOR, only("Title", "t", "head", hd("x")), CodeUnknownField},
		{"case-DRAFT", same, repoOR, ok("DRAFT", false), CodeUnknownField},
		{"space-in-key", same, repoOR, only("title ", "t", "head", hd("x")), CodeUnknownField},
		{"nul-in-key", same, repoOR, only("title\x00", "t", "head", hd("x")), CodeUnknownField},
		{"empty-key", same, repoOR, only("", "t", "title", "t", "head", hd("x")), CodeUnknownField},
		{"dup-title", same, repoOR, `{"title":"a","title":"b","head":"` + hd("x") + `"}`, CodeDuplicateField},
		{"dup-title-escaped", same, repoOR, `{"title":"a","t\u0069tle":"b","head":"` + hd("x") + `"}`, CodeDuplicateField},
		{"dup-draft", same, repoOR, ok("draft", true) + "", ""}, // 下で差し替える
		{"dup-head", same, repoOR, `{"title":"a","head":"` + hd("x") + `","head":"` + hd("y") + `"}`, CodeDuplicateField},
		// 値の型
		{"missing-title", same, repoOR, only("head", hd("x")), CodeBadField},
		{"missing-head", same, repoOR, only("title", "t"), CodeBadField},
		{"title-null", same, repoOR, `{"title":null,"head":"` + hd("x") + `"}`, CodeBadField},
		{"title-number", same, repoOR, only("title", 5, "head", hd("x")), CodeBadField},
		{"title-array", same, repoOR, only("title", []string{"t"}, "head", hd("x")), CodeBadField},
		{"title-object", same, repoOR, only("title", map[string]string{}, "head", hd("x")), CodeBadField},
		{"title-bool", same, repoOR, only("title", true, "head", hd("x")), CodeBadField},
		{"body-null", same, repoOR, `{"title":"t","body":null,"head":"` + hd("x") + `"}`, CodeBadField},
		{"body-number", same, repoOR, ok("body", 1), CodeBadField},
		{"head-null", same, repoOR, `{"title":"t","head":null}`, CodeBadField},
		{"base-null", same, repoOR, `{"title":"t","head":"` + hd("x") + `","base":null}`, CodeBadField},
		{"base-empty", same, repoOR, ok("base", ""), CodeBadField},
		{"draft-null", same, repoOR, `{"title":"t","head":"` + hd("x") + `","base":"main","draft":null}`, CodeBadField},
		{"draft-string", same, repoOR, ok("draft", "true"), CodeBadField},
		{"draft-number", same, repoOR, ok("draft", 1), CodeBadField},
		{"draft-array", same, repoOR, ok("draft", []bool{true}), CodeBadField},
		{"draft-object", same, repoOR, ok("draft", map[string]bool{}), CodeBadField},
		// title・body の中身
		{"title-empty", same, repoOR, ok("title", ""), CodeBadField},
		{"title-blank", same, repoOR, ok("title", "   "), CodeBadField},
		{"title-space-tab", same, repoOR, ok("title", " \t "), CodeBadField},
		{"title-257", same, repoOR, ok("title", strings.Repeat("a", MaxTitleBytes+1)), CodeBadField},
		{"title-multibyte-over", same, repoOR, ok("title", strings.Repeat("あ", 86)), CodeBadField}, // 258 バイト
		{"title-lf", same, repoOR, ok("title", "a\nb"), CodeBadField},
		{"title-tab", same, repoOR, ok("title", "a\tb"), CodeBadField},
		{"title-cr", same, repoOR, ok("title", "a\rb"), CodeBadField},
		{"title-nul", same, repoOR, ok("title", "a\x00b"), CodeBadField},
		{"title-esc", same, repoOR, ok("title", "\x1b[2Jx"), CodeBadField},
		{"title-del", same, repoOR, ok("title", "a\x7fb"), CodeBadField},
		{"title-c1", same, repoOR, ok("title", "a\u0085b"), CodeBadField},
		{"title-ls", same, repoOR, ok("title", "a\u2028b"), CodeBadField},
		{"title-ps", same, repoOR, ok("title", "a\u2029b"), CodeBadField},
		{"title-bom", same, repoOR, ok("title", "a\ufeffb"), CodeBadField},
		{"title-rlo", same, repoOR, ok("title", "a\u202eb"), CodeBadField},
		{"title-lri", same, repoOR, ok("title", "a\u2066b"), CodeBadField},
		{"title-pdi", same, repoOR, ok("title", "a\u2069b"), CodeBadField},
		{"title-202a", same, repoOR, ok("title", "a\u202ab"), CodeBadField},
		{"title-202b", same, repoOR, ok("title", "a\u202bb"), CodeBadField},
		{"title-202c", same, repoOR, ok("title", "a\u202cb"), CodeBadField},
		{"title-202d", same, repoOR, ok("title", "a\u202db"), CodeBadField},
		{"title-2067", same, repoOR, ok("title", "a\u2067b"), CodeBadField},
		{"title-2068", same, repoOR, ok("title", "a\u2068b"), CodeBadField},
		{"body-65537", same, repoOR, ok("body", strings.Repeat("b", MaxBodyBytes+1)), CodeBadField},
		{"body-nul", same, repoOR, ok("body", "a\x00b"), CodeBadField},
		{"body-esc", same, repoOR, ok("body", "a\x1bb"), CodeBadField},
		{"body-del", same, repoOR, ok("body", "a\x7fb"), CodeBadField},
		{"body-vt", same, repoOR, ok("body", "a\x0bb"), CodeBadField},
		{"body-ff", same, repoOR, ok("body", "a\x0cb"), CodeBadField},
		{"body-ls", same, repoOR, ok("body", "a\u2028b"), CodeBadField},
		{"body-rlo", same, repoOR, ok("body", "a\u202eb"), CodeBadField},
		{"body-bom", same, repoOR, ok("body", "\ufeffa"), CodeBadField},
		// base
		{"base-dash", same, repoOR, ok("base", "-x"), CodeBadField},
		{"base-space", same, repoOR, ok("base", "a b"), CodeBadField},
		{"base-dotdot", same, repoOR, ok("base", "a..b"), CodeBadField},
		{"base-lock", same, repoOR, ok("base", "x.lock"), CodeBadField},
		{"base-colon", same, repoOR, ok("base", "o:main"), CodeBadField},
		{"base-nl", same, repoOR, ok("base", "main\n"), CodeBadField},
		{"base-201", same, repoOR, ok("base", strings.Repeat("a", 201)), CodeBadField},
		// head
		{"head-main", same, repoOR, ok("head", "main"), CodeHeadNotAllowed},
		{"head-other-session", same, repoOR, ok("head", "goro/20260927-041500-ffffff/x"), CodeHeadNotAllowed},
		{"head-session-only", same, repoOR, ok("head", "goro/"+sess), CodeHeadNotAllowed},
		{"head-session-slash", same, repoOR, ok("head", "goro/"+sess+"/"), CodeHeadNotAllowed},
		{"head-dotdot", same, repoOR, ok("head", hd("../x")), CodeHeadNotAllowed},
		{"head-full-ref", same, repoOR, ok("head", "refs/heads/"+hd("x")), CodeHeadNotAllowed},
		{"head-colon-suffix", same, repoOR, ok("head", hd("x:y")), CodeHeadNotAllowed},
		{"head-only-owner", same, repoOR, ok("head", "o:x"), CodeHeadNotAllowed},
		{"head-other-owner", same, repoOR, ok("head", "x:"+hd("x")), CodeHeadNotAllowed},
		{"head-double-owner", same, repoOR, ok("head", "o:o:"+hd("x")), CodeHeadNotAllowed},
		{"head-empty-owner", same, repoOR, ok("head", ":"+hd("x")), CodeHeadNotAllowed},
		{"head-empty", same, repoOR, ok("head", ""), CodeHeadNotAllowed},
		{"head-nl", same, repoOR, ok("head", hd("x")+"\n"), CodeHeadNotAllowed},
		{"head-space", same, repoOR, ok("head", " "+hd("x")), CodeHeadNotAllowed},
		{"head-dot", same, repoOR, ok("head", hd(".x")), CodeHeadNotAllowed},
		{"head-lock", same, repoOR, ok("head", hd("x.lock")), CodeHeadNotAllowed},
		{"head-long", same, repoOR, ok("head", hd(strings.Repeat("a", 300))), CodeHeadNotAllowed},
		{"head-unicode", same, repoOR, ok("head", hd("日本語")), CodeHeadNotAllowed},
		{"fork-plain-head", fork, repoUp, ok("head", hd("x")), CodeHeadNotAllowed},
		{"fork-other-owner", fork, repoUp, ok("head", "other:"+hd("x")), CodeHeadNotAllowed},
		{"fork-upstream-owner", fork, repoUp, ok("head", "up:"+hd("x")), CodeHeadNotAllowed},
		{"fork-main", fork, repoUp, ok("head", "me:main"), CodeHeadNotAllowed},
		{"fork-other-session", fork, repoUp, ok("head", "me:goro/20260927-041500-ffffff/x"), CodeHeadNotAllowed},
	}
	// dup-draft の本文は、重複を書いたものにする。
	for i := range cases {
		if cases[i].name == "dup-draft" {
			cases[i].body = `{"title":"t","head":"` + hd("x") + `","draft":true,"draft":false}`
			cases[i].code = CodeDuplicateField
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pull, err := c.pp.ParsePull(c.repo, []byte(c.body))
			if err == nil {
				t.Fatalf("断るはず: %+v", pull)
			}
			if Reason(err) != c.code {
				t.Fatalf("Code = %q, 期待 %q (%v)", Reason(err), c.code, err)
			}
			if !safeText(err.Error()) || len(err.Error()) > 400 {
				t.Fatalf("エラーの文字列が安全でない: %q", err.Error())
			}
		})
	}
}

// TestParsePullMissingField は、必須の項目が無いとき、無い項目の名前を、エラーの文字列で示すことを確かめる。
func TestParsePullMissingField(t *testing.T) {
	pp := policyFor(t, repoOR)
	for field, body := range map[string]string{
		"title": pullBody(t, "head", headOf("x")),
		"head":  pullBody(t, "title", "t"),
	} {
		_, err := pp.ParsePull(repoOR, []byte(body))
		if Reason(err) != CodeBadField || !strings.Contains(err.Error(), field+" が無い") {
			t.Errorf("%s が無い: %v", field, err)
		}
	}
}

// TestParsePullErrorsHideHostileText は、攻撃者の文字 (制御文字・書式制御・別の文字・かな漢字) が、エラーの文字列に、そのまま入らないことを確かめる。
func TestParsePullErrorsHideHostileText(t *testing.T) {
	const hostile = "\u202e\u200b\u2028ж龘😀\x1b\x07"
	pp := policyFor(t, repoOR)
	for _, body := range []string{
		pullBody(t, hostile, 1),
		pullBody(t, "title", "t", "head", hostile),
		pullBody(t, "title", "t", "head", headOf("x"), "base", hostile),
		pullBody(t, "title", "t", "head", hostile+":"+headOf("x")),
	} {
		_, err := pp.ParsePull(repoOR, []byte(body))
		if err == nil {
			t.Fatalf("通った: %q", body)
		}
		if !safeText(err.Error()) || strings.ContainsAny(err.Error(), hostile) {
			t.Fatalf("エラーの文字列が安全でない: %q", err.Error())
		}
	}
	_, err := pp.ParsePull(git.Repo{Owner: hostile, Name: hostile}, []byte("{}"))
	if err == nil || !safeText(err.Error()) || strings.ContainsAny(err.Error(), hostile) {
		t.Fatalf("エラーの文字列が安全でない: %v", err)
	}
}

func TestWithBase(t *testing.T) {
	p := Pull{Repo: repoOR, Title: "t", Head: headOf("x")}
	for _, ok := range []string{"main", "release/1.0"} {
		if got, err := p.WithBase(ok); err != nil || got.Base != ok {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "a..b", "x\n"} {
		if _, err := p.WithBase(bad); Reason(err) != CodeBadField {
			t.Errorf("%q は断るはず: %v", bad, err)
		}
	}
	if p.Base != "" {
		t.Errorf("元の Pull を書き換えた")
	}
}

var headShape = regexp.MustCompile(`^((me|o):)?goro/` + regexp.QuoteMeta(sess) + `/[A-Za-z0-9._/-]+$`)

// FuzzParsePull は、任意の本文で、パニックせず、通したものが次を満たすことを確かめる:
// 上流に送る JSON は、固定の 5 項目だけで draft が true・head が許可した形・文字が規則の内側にある。
// 別の実装 (encoding/json で map に読む) と、title・body・head の値が一致し、そちらでは、項目が全て許可した名前 (完全一致) で、重複が無い。
func FuzzParsePull(f *testing.F) {
	h := headOf("x")
	for _, s := range []string{
		"", "{}", "[]", `{"title":"t","head":"` + h + `","base":"main"}`, `{"title":"t","head":"` + h + `","draft":false}`,
		`{"title":"t","title":"u","head":"` + h + `"}`, `{"Title":"t","head":"` + h + `"}`, `{"title":"t","head":"o:` + h + `","base":"main","body":"a\nb"}`,
		`{"title":"t\u202e","head":"` + h + `"}`, `{"title":"a","t\u0069tle":"b","head":"` + h + `"}`, `{"x":[[[[[[]]]]]]}`,
	} {
		f.Add(s)
	}
	pp := policyFor(f, repoOR)
	f.Fuzz(func(t *testing.T, s string) {
		pull, err := pp.ParsePull(repoOR, []byte(s))
		if err != nil {
			if !safeText(err.Error()) || len(err.Error()) > 400 {
				t.Fatalf("エラーの文字列が安全でない: %q", err.Error())
			}
			return
		}
		if pull.Base == "" {
			var werr error
			if pull, werr = pull.WithBase("main"); werr != nil {
				t.Fatal(werr)
			}
		}
		out, err := pull.JSON()
		if err != nil {
			t.Fatal(err)
		}
		// 上流に送る本文: 5 項目だけ、draft は true。
		var sent map[string]any
		if err := json.Unmarshal(out, &sent); err != nil {
			t.Fatalf("JSON でない: %v", err)
		}
		for k := range sent {
			if !allowedFields[k] {
				t.Fatalf("許可していない項目を送る: %q", k)
			}
		}
		if sent["draft"] != true || sent["title"] == "" || !headShape.MatchString(fmt.Sprint(sent["head"])) {
			t.Fatalf("送る本文が規則の外: %s", out)
		}
		if len(pull.Title) > MaxTitleBytes || len(pull.Body) > MaxBodyBytes || strings.TrimSpace(pull.Title) == "" {
			t.Fatalf("大きさ・空白の規則の外")
		}
		for _, r := range pull.Title + pull.Body {
			if r == 0x2028 || r == 0x2029 || r == 0xfeff || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || unicode.Is(unicode.Cc, r) && !(r == '\n' || r == '\r' || r == '\t') {
				t.Fatalf("使えない文字を通した: %U", r)
			}
		}
		if strings.ContainsAny(pull.Title, "\n\r\t") {
			t.Fatalf("title に改行かタブ")
		}
		// 独立した実装 (標準の map への読み込み) と、値が一致する。標準の読み方で、重複や大文字違いがあれば、こちらが通すはずがない。
		var std map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &std); err != nil {
			t.Fatalf("標準の実装が読めないものを通した: %v", err)
		}
		for k := range std {
			if !allowedFields[k] {
				t.Fatalf("標準の実装では、許可していない項目 %q がある", k)
			}
		}
		var title string
		if err := json.Unmarshal(std["title"], &title); err != nil || title != pull.Title {
			t.Fatalf("title が標準の実装と違う: %q %q (%v)", title, pull.Title, err)
		}
		if raw, ok := std["body"]; ok {
			var body string
			if err := json.Unmarshal(raw, &body); err != nil || body != pull.Body {
				t.Fatalf("body が標準の実装と違う")
			}
		}
	})
}

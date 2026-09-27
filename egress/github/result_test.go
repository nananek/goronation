package github

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestParseResult(t *testing.T) {
	// 実際の応答は、これに、大量の項目が付く。
	full := func(number, url, draft string) string {
		return `{"url":"https://api.github.com/repos/o/r/pulls/5","id":1,"number":` + number + `,"state":"open","title":"secret title",` +
			`"html_url":"` + url + `","user":{"login":"x","token":"ghp_shouldnotleak"},"draft":` + draft + `}`
	}
	ok := full("5", "https://github.com/o/r/pull/5", "true")
	got, err := ParseResult([]byte(ok), repoOR)
	if err != nil || got != (Result{Number: 5, URL: "https://github.com/o/r/pull/5", Draft: true}) {
		t.Fatalf("%+v %v", got, err)
	}
	if s := string(got.JSON()); s != `{"number":5,"html_url":"https://github.com/o/r/pull/5"}` {
		t.Fatalf("JSON = %s", s)
	}
	// 上流の綴りが違っても (大文字小文字)、返す URL は、許可した綴りで作り直す。
	got, err = ParseResult([]byte(full("7", "https://github.com/O/R/pull/7", "true")), repoOR)
	if err != nil || got.URL != "https://github.com/o/r/pull/7" {
		t.Fatalf("%+v %v", got, err)
	}
	// draft が、false・無いとき: 通すが、Draft は false (呼び手が、監査に残す)。
	for _, d := range []string{"false", "null"} {
		got, err = ParseResult([]byte(full("5", "https://github.com/o/r/pull/5", d)), repoOR)
		if err != nil || got.Draft {
			t.Fatalf("draft=%s: %+v %v", d, got, err)
		}
	}
	got, err = ParseResult([]byte(`{"number":5,"html_url":"https://github.com/o/r/pull/5"}`), repoOR)
	if err != nil || got.Draft {
		t.Fatalf("draft 無し: %+v %v", got, err)
	}

	bad := map[string]string{
		"number-mismatch":  full("5", "https://github.com/o/r/pull/6", "true"),
		"number-zero":      full("0", "https://github.com/o/r/pull/0", "true"),
		"number-negative":  full("-1", "https://github.com/o/r/pull/-1", "true"),
		"number-float":     full("5.5", "https://github.com/o/r/pull/5", "true"),
		"number-string":    full(`"5"`, "https://github.com/o/r/pull/5", "true"),
		"other-repo":       full("5", "https://github.com/x/y/pull/5", "true"),
		"other-host":       full("5", "https://evil.example/o/r/pull/5", "true"),
		"host-suffix":      full("5", "https://github.com.evil.example/o/r/pull/5", "true"),
		"userinfo":         full("5", "https://github.com@evil.example/o/r/pull/5", "true"),
		"http":             full("5", "http://github.com/o/r/pull/5", "true"),
		"query":            full("5", "https://github.com/o/r/pull/5?x=1", "true"),
		"fragment":         full("5", "https://github.com/o/r/pull/5#x", "true"),
		"trailing-slash":   full("5", "https://github.com/o/r/pull/5/", "true"),
		"issues":           full("5", "https://github.com/o/r/issues/5", "true"),
		"escape-in-url":    full("5", "https://github.com/o/r/pull/5\\u001b[2J", "true"),
		"no-html_url":      `{"number":5}`,
		"no-number":        `{"html_url":"https://github.com/o/r/pull/5"}`,
		"html_url-null":    `{"number":5,"html_url":null}`,
		"html_url-number":  `{"number":5,"html_url":5}`,
		"array":            `[]`,
		"empty":            ``,
		"not-json":         `<html>`,
		"error-body":       `{"message":"Validation Failed","errors":[{"resource":"PullRequest"}]}`,
		"trailing-garbage": ok + "x",
	}
	for name, body := range bad {
		if got, err := ParseResult([]byte(body), repoOR); err == nil || Reason(err) != CodeBadResponse {
			t.Errorf("%s: 断るはず: %+v %v", name, got, err)
		} else if !safeText(err.Error()) {
			t.Errorf("%s: エラーの文字列が安全でない: %q", name, err.Error())
		}
	}
	if _, err := ParseResult([]byte(strings.Repeat(" ", MaxResponseBytes+1)), repoOR); Reason(err) != CodeTooLarge {
		t.Errorf("大きい応答: %v", err)
	}
	// 檻に返す JSON に、上流の他の値 (title・user・token) は入らない。
	if s := string(got.JSON()); strings.Contains(s, "secret") || strings.Contains(s, "ghp_") || strings.Contains(s, "user") {
		t.Errorf("JSON に、上流の他の値が入った: %s", s)
	}
}

func TestDefaultBranch(t *testing.T) {
	for body, want := range map[string]string{
		`{"default_branch":"main","private":true}`:      "main",
		`{"default_branch":"release/1.0"}`:              "release/1.0",
		`{"name":"r","default_branch":"develop","x":1}`: "develop",
	} {
		if got, err := DefaultBranch([]byte(body)); err != nil || got != want {
			t.Errorf("%s: %q %v", body, got, err)
		}
	}
	for _, body := range []string{
		``, `{}`, `[]`, `null`, `{"default_branch":null}`, `{"default_branch":""}`, `{"default_branch":5}`, `{"default_branch":"a b"}`,
		`{"default_branch":"-x"}`, `{"default_branch":"a..b"}`, `{"default_branch":"main\n"}`, `{"default_branch":"` + strings.Repeat("a", 201) + `"}`,
		`{"message":"Not Found"}`, `<html>`,
	} {
		if got, err := DefaultBranch([]byte(body)); Reason(err) != CodeBadResponse {
			t.Errorf("%q: 断るはず: %q %v", body, got, err)
		}
	}
	if _, err := DefaultBranch([]byte(strings.Repeat(" ", MaxResponseBytes+1))); Reason(err) != CodeTooLarge {
		t.Errorf("大きい応答: %v", err)
	}
}

func TestQuota(t *testing.T) {
	q := NewQuota(2)
	if q.Take() != nil || q.Take() != nil {
		t.Fatal("2 回は使えるはず")
	}
	if err := q.Take(); Reason(err) != CodeQuota {
		t.Fatalf("3 回目: %v", err)
	}
	for _, n := range []int{0, -1, -100} {
		if err := NewQuota(n).Take(); Reason(err) != CodeQuota {
			t.Errorf("NewQuota(%d) は、1 回も許さない: %v", n, err)
		}
	}
	// 並行して使っても、枠より多くは通らない。
	q = NewQuota(5)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if q.Take() == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 {
		t.Fatalf("通ったのは %d 回 (枠は 5)", ok.Load())
	}
}

// FuzzParseResult は、任意の応答で、パニックせず、通したものが、number と repo から作り直した URL だけを返すことを確かめる。
func FuzzParseResult(f *testing.F) {
	for _, s := range []string{`{"number":5,"html_url":"https://github.com/o/r/pull/5","draft":true}`, `{}`, `[]`, ``, `{"number":1e2,"html_url":"https://github.com/o/r/pull/100"}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseResult([]byte(s), repoOR)
		if err != nil {
			if !safeText(err.Error()) {
				t.Fatalf("エラーが安全でない: %q", err.Error())
			}
			return
		}
		want := "https://github.com/o/r/pull/" + strconv.Itoa(r.Number)
		if r.Number < 1 || r.URL != want {
			t.Fatalf("結果が規則の外: %+v", r)
		}
		if string(r.JSON()) != `{"number":`+strconv.Itoa(r.Number)+`,"html_url":"`+want+`"}` {
			t.Fatalf("JSON = %s", r.JSON())
		}
	})
}

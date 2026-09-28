//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/goronation/cmd/internal/credfile"
	"github.com/nananek/goronation/core/credential"
	"github.com/nananek/goronation/egress/gateway"
)

// saveTestToken は、stateDir に、gateway.CredentialName ("github") のトークン tok を保存する
// (credfile.New(stateDir).Save と同じ経路。goro auth github が使うのと同じ形)。
func saveTestToken(t *testing.T, stateDir, tok string) {
	t.Helper()
	store, err := credfile.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(gateway.CredentialName, credential.New(tok)); err != nil {
		t.Fatal(err)
	}
}

// writeGitHead は、dir/.git/HEAD (と dir/.git) を作り、content を書く。
func writeGitHead(t *testing.T, dir, content string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentBranch(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, "ref: refs/heads/goro/sess-1/feature\n")
	got, err := currentBranch(dir)
	if err != nil {
		t.Fatalf("currentBranch: %v", err)
	}
	if want := "goro/sess-1/feature"; got != want {
		t.Fatalf("currentBranch = %q, want %q", got, want)
	}
}

func TestCurrentBranchFromSubdir(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, "ref: refs/heads/main\n")
	sub := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := currentBranch(sub)
	if err != nil {
		t.Fatalf("currentBranch (subdir): %v", err)
	}
	if got != "main" {
		t.Fatalf("currentBranch = %q, want main", got)
	}
}

func TestCurrentBranchDetachedHEAD(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, "e5cd029082061e5fea3bd87cef163c282655fe7c\n")
	if _, err := currentBranch(dir); err == nil {
		t.Fatal("detached HEAD なのに error にならなかった")
	}
}

func TestCurrentBranchNoGitDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := currentBranch(dir); err == nil {
		t.Fatal(".git が無いのに error にならなかった")
	}
}

func TestCurrentBranchOversizedHead(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, strings.Repeat("a", maxGitHeadBytes+1))
	if _, err := currentBranch(dir); err == nil {
		t.Fatal("大きすぎる .git/HEAD なのに error にならなかった")
	}
}

func TestRunPrCreateRequiresTitle(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runPrCreate(nil, &out, &errOut); code != exitUsage {
		t.Fatalf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errOut.String(), "--title") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunPrCreateRequiresGoroPushRepo(t *testing.T) {
	t.Setenv("GORO_PUSH_REPO", "")
	var out, errOut bytes.Buffer
	code := runPrCreate([]string{"--title", "t"}, &out, &errOut)
	if code == 0 {
		t.Fatal("GORO_PUSH_REPO が無いのに成功した")
	}
	if !strings.Contains(errOut.String(), "GORO_PUSH_REPO") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunPrCreateRejectsBadGoroPushRepo(t *testing.T) {
	t.Setenv("GORO_PUSH_REPO", "not-a-repo")
	var out, errOut bytes.Buffer
	code := runPrCreate([]string{"--title", "t"}, &out, &errOut)
	if code == 0 {
		t.Fatal("壊れた GORO_PUSH_REPO なのに成功した")
	}
	if !strings.Contains(errOut.String(), "owner/repo") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// TestRunPrCreateSendsExpectedJSON は、実際に (jailAPIBase の代わりに loopback の偽サーバへ) 要求を送る経路を
// 直接は使わず、payload の組み立て (title・head・body・base の有無) だけを確かめる代わりに、findGitDir 経由で
// currentBranch が読めない (.git が無い) ケースで、要求を一切送らずに断ることを確かめる (ネットワークに触れない)。
func TestRunPrCreateFailsWithoutGitDir(t *testing.T) {
	t.Setenv("GORO_PUSH_REPO", "o/r")
	chdir(t, t.TempDir())
	var out, errOut bytes.Buffer
	code := runPrCreate([]string{"--title", "t"}, &out, &errOut)
	if code == 0 {
		t.Fatal(".git が無いのに成功した")
	}
	if !strings.Contains(errOut.String(), "ブランチ") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunPrReadyRequiresTwoArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runPrReady([]string{"o/r"}, &out, &errOut); code != exitUsage {
		t.Fatalf("code = %d, want exitUsage", code)
	}
}

func TestRunPrReadyRejectsBadRepo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runPrReady([]string{"not-a-repo", "1"}, &out, &errOut); code != exitUsage {
		t.Fatalf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errOut.String(), "owner/repo") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunPrReadyRejectsBadNumber(t *testing.T) {
	for _, n := range []string{"0", "-1", "abc", ""} {
		var out, errOut bytes.Buffer
		if code := runPrReady([]string{"o/r", n}, &out, &errOut); code != exitUsage {
			t.Errorf("番号 %q: code = %d, want exitUsage", n, code)
		}
	}
}

// withFakeGraphQL は、ghapiBaseURL (var) を、テストの間だけ handler の httptest サーバに差し替える。
func withFakeGraphQL(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := ghapiBaseURL
	ghapiBaseURL = srv.URL
	t.Cleanup(func() { ghapiBaseURL = old })
}

func TestRunPrReadyRequiresToken(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := runPrReady([]string{"o/r", "1", "--state-dir", dir}, &out, &errOut)
	if code == 0 {
		t.Fatal("トークンが無いのに成功した")
	}
	if !strings.Contains(errOut.String(), "goro auth github") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// TestRunPrReadyFullFlow は、goro pr ready が、保存済みトークンを読んで GraphQL を呼び、checks が SUCCESS
// の PR を ready for review にすることを、偽の GraphQL 上流に対して確かめる。
func TestRunPrReadyFullFlow(t *testing.T) {
	dir := t.TempDir()
	saveTestToken(t, dir, "tok-123")

	var gotAuth string
	withFakeGraphQL(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "markPullRequestReadyForReview") {
			io.WriteString(w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_1","isDraft":false}}}}`)
			return
		}
		io.WriteString(w, `{"data":{"repository":{"pullRequest":{"id":"PR_1","isDraft":true,"headRefOid":"deadbeef","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}}}}`)
	})

	var out, errOut bytes.Buffer
	code := runPrReady([]string{"o/r", "7", "--state-dir", dir}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	if gotAuth != "bearer tok-123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(out.String(), "o/r#7") || !strings.Contains(out.String(), "ready for review") {
		t.Fatalf("stdout = %q", out.String())
	}
}

// TestRunPrReadyRefusesWhenChecksNotGreen は、checks が SUCCESS でなければ、mutation を呼ばずに断ることを
// 確かめる。
func TestRunPrReadyRefusesWhenChecksNotGreen(t *testing.T) {
	dir := t.TempDir()
	saveTestToken(t, dir, "tok-123")

	mutationCalled := false
	withFakeGraphQL(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "markPullRequestReadyForReview") {
			mutationCalled = true
			io.WriteString(w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_1","isDraft":false}}}}`)
			return
		}
		io.WriteString(w, `{"data":{"repository":{"pullRequest":{"id":"PR_1","isDraft":true,"headRefOid":"deadbeef","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING"}}}]}}}}}`)
	})

	var out, errOut bytes.Buffer
	code := runPrReady([]string{"o/r", "7", "--state-dir", dir}, &out, &errOut)
	if code == 0 {
		t.Fatal("checks が PENDING なのに成功した")
	}
	if mutationCalled {
		t.Fatal("checks が PENDING なのに mutation を呼んだ")
	}
}

// withFakeJailAPI は、jailAPIBase (var) を、テストの間だけ handler の httptest サーバに差し替える。
func withFakeJailAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := jailAPIBase
	jailAPIBase = srv.URL + "/"
	t.Cleanup(func() { jailAPIBase = old })
}

// chdir は、テストの間だけ、カレントディレクトリを dir にする。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

// TestRunPrCreateFullFlow は、runPrCreate が、egress/gateway の応答の形 (201 + {"number","html_url"}) を
// 模した偽の上流に対して、正しい JSON (title・head。draft は送らない。上流が常に上書きするため) を送り、
// 成功のメッセージを stdout に出すことを確かめる。
func TestRunPrCreateFullFlow(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, "ref: refs/heads/goro/sess-1/x\n")
	chdir(t, dir)
	t.Setenv("GORO_PUSH_REPO", "o/r")

	var gotPath string
	var gotBody map[string]any
	withFakeJailAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42"}`)
	})

	var out, errOut bytes.Buffer
	code := runPrCreate([]string{"--title", "t", "--body", "b"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	if gotPath != "/repos/o/r/pulls" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["title"] != "t" || gotBody["head"] != "goro/sess-1/x" || gotBody["body"] != "b" {
		t.Fatalf("body = %+v", gotBody)
	}
	if _, hasDraft := gotBody["draft"]; hasDraft {
		t.Fatal("draft を送ってはいけない (上流が常に true に上書きするため、クライアントは送らない)")
	}
	if _, hasBase := gotBody["base"]; hasBase {
		t.Fatal("--base を指定していないのに base を送った")
	}
	if !strings.Contains(out.String(), "#42") || !strings.Contains(out.String(), "pull/42") {
		t.Fatalf("stdout = %q", out.String())
	}
}

// TestRunPrCreateUpstreamRejects は、上流 (gateway) が非 201 (403 など) を返したとき、goro pr create が
// error として断り、本文を stderr に (制御文字などを無害化して) 出すことを確かめる。
func TestRunPrCreateUpstreamRejects(t *testing.T) {
	dir := t.TempDir()
	writeGitHead(t, dir, "ref: refs/heads/main\n") // Push の名前空間の外: 実際の gateway なら断る想定を模す
	chdir(t, dir)
	t.Setenv("GORO_PUSH_REPO", "o/r")

	withFakeJailAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	})

	var out, errOut bytes.Buffer
	code := runPrCreate([]string{"--title", "t"}, &out, &errOut)
	if code == 0 {
		t.Fatal("上流が 403 なのに成功した")
	}
	if !strings.Contains(errOut.String(), "403") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

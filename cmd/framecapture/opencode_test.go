//go:build linux

package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nananek/goronation/tools/fakeproviders/openai"
)

func TestWriteOpencodeConfigBaseURL(t *testing.T) {
	dir := t.TempDir()
	if err := writeOpencodeConfig(dir); err != nil {
		t.Fatalf("writeOpencodeConfig: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cfg struct {
		Provider map[string]struct {
			NPM     string `json:"npm"`
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p, ok := cfg.Provider[opencodeProviderName]
	if !ok {
		t.Fatalf("provider %q が無い: %+v", opencodeProviderName, cfg)
	}
	if p.NPM != "@ai-sdk/openai-compatible" {
		t.Errorf("npm = %q", p.NPM)
	}
	wantBaseURL := "https://" + opencodeFakeHost + "/v1"
	if p.Options.BaseURL != wantBaseURL {
		t.Errorf("baseURL = %q, want %q", p.Options.BaseURL, wantBaseURL)
	}
	if _, ok := p.Models[opencodeModelName]; !ok {
		t.Errorf("models に %q が無い: %+v", opencodeModelName, p.Models)
	}
}

func TestOpencodeArgsFirstTurn(t *testing.T) {
	args := opencodeArgs(false, []string{"hello", "world"})
	want := []string{"run", "--format", "json", "-m", opencodeModelRef, "hello", "world"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %+v, want %+v", args, want)
	}
}

func TestOpencodeArgsContinue(t *testing.T) {
	args := opencodeArgs(true, []string{"hello"})
	want := []string{"run", "--format", "json", "-m", opencodeModelRef, "--continue", "hello"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %+v, want %+v", args, want)
	}
}

func TestIsTitleRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"タイトル生成":      {`{"messages":[{"role":"system","content":"You are a title generator. You output ONLY a thread title."},{"role":"user","content":"x"}]}`, true},
		"tools 付きの本体": {`{"messages":[{"role":"system","content":"You are a title generator."}],"tools":[{"type":"function"}]}`, false},
		"通常の system":  {`{"messages":[{"role":"system","content":"You are opencode."}]}`, false},
		"先頭が user":    {`{"messages":[{"role":"user","content":"You are a title generator"}]}`, false},
		"content が配列": {`{"messages":[{"role":"system","content":[{"type":"text"}]}]}`, false},
		"messages が空": {`{"messages":[]}`, false},
		"JSON でない":    {`nope`, false},
	} {
		if got := isTitleRequest([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: isTitleRequest = %v, want %v", name, got, tc.want)
		}
	}
}

func TestWithTitleRequestsDoesNotConsumeScenarioSteps(t *testing.T) {
	scenarioSrv := openai.NewServer(openai.Step{Content: "STEP-ONE"}, openai.Step{Content: "STEP-TWO"})
	h := withTitleRequests(scenarioSrv)

	post := func(body string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		return rec.Body.String()
	}
	titleReq := `{"stream":false,"messages":[{"role":"system","content":"You are a title generator."}]}`
	mainReq := `{"stream":false,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read"}}]}`

	if got := post(titleReq); !strings.Contains(got, opencodeTitleReply) || strings.Contains(got, "STEP-") {
		t.Fatalf("タイトル生成への応答 = %s", got)
	}
	if got := post(mainReq); !strings.Contains(got, "STEP-ONE") {
		t.Fatalf("本体の 1 つ目の応答が STEP-ONE でない (タイトル生成が Step を食った?): %s", got)
	}
	if got := post(mainReq); !strings.Contains(got, "STEP-TWO") {
		t.Fatalf("本体の 2 つ目の応答 = %s", got)
	}
	if n := len(scenarioSrv.Requests()); n != 2 {
		t.Fatalf("場面の fake が受けたリクエスト = %d, want 2 (タイトル生成は含まない)", n)
	}
}

//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
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

func TestOpencodeArgsWithoutSession(t *testing.T) {
	args := opencodeArgs("", []string{"hello", "world"})
	want := []string{"run", "--format", "json", "-m", opencodeModelRef, "hello", "world"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %+v, want %+v", args, want)
	}
}

func TestOpencodeArgsWithSession(t *testing.T) {
	args := opencodeArgs("ses_abc", []string{"hello"})
	want := []string{"run", "--format", "json", "-m", opencodeModelRef, "--session", "ses_abc", "hello"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %+v, want %+v", args, want)
	}
}

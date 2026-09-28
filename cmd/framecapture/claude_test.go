//go:build linux

package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestClaudeArgsIncludesStreamJSONAndSettings(t *testing.T) {
	args := claudeArgs()
	want := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--settings", claudeAPIKeyHelperSettings,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %+v, want %+v", args, want)
	}
	// --settings の値が妥当な JSON で、apiKeyHelper を持つことを確かめる (資格情報らしい名前の環境変数を
	// 使わずに済ませるための、唯一の配線経路なので、壊れていないことが重要)。
	var settings struct {
		APIKeyHelper string `json:"apiKeyHelper"`
	}
	if err := json.Unmarshal([]byte(claudeAPIKeyHelperSettings), &settings); err != nil {
		t.Fatalf("claudeAPIKeyHelperSettings が妥当な JSON でない: %v", err)
	}
	if settings.APIKeyHelper == "" {
		t.Fatal("apiKeyHelper が空")
	}
}

func TestClaudeEnvHasNoCredentialLikeKeys(t *testing.T) {
	// sandbox/bwrap の Argv 検証が拒む「資格情報らしい名前」を、うっかり環境変数に混ぜていないかの回帰
	// 確認 (ANTHROPIC_API_KEY を混ぜて bwrap: Env[...]: 資格情報らしい名前 で落ちた実績があるため)。
	credParts := []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "PRIVATE_KEY", "ACCESS_KEY"}
	for _, e := range claudeEnv() {
		upper := strings.ToUpper(e.Key)
		for _, part := range credParts {
			if strings.Contains(upper, part) {
				t.Errorf("claudeEnv() の %q が、資格情報らしい名前を含む (%q)", e.Key, part)
			}
		}
	}
}

func TestClaudeStreamJSONLineShape(t *testing.T) {
	line := claudeStreamJSONLine("hello there")
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "user" || got.Message.Role != "user" {
		t.Fatalf("got = %+v", got)
	}
	if len(got.Message.Content) != 1 || got.Message.Content[0].Type != "text" || got.Message.Content[0].Text != "hello there" {
		t.Fatalf("content = %+v", got.Message.Content)
	}
}

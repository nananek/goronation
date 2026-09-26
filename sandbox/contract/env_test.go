package contract

import (
	"slices"
	"strings"
	"testing"
)

// TestCheckEnv は、環境変数の名前と値の規則を確認する。
func TestCheckEnv(t *testing.T) {
	for _, k := range []string{"HOME", "PATH", "TERM", "LANG", "_X", "a1", "OPENCODE_DISABLE_SHARE", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_TELEMETRY", "NO_PROXY"} {
		if err := CheckEnv(k, "v"); err != nil {
			t.Errorf("CheckEnv(%q) = %v, want nil", k, err)
		}
	}
	for k, want := range map[string]string{
		"": "名前", "1A": "名前", "A B": "名前", "A=B": "名前", "A\x00B": "名前", "A-B": "名前", "名前": "名前",
		"SSH_AUTH_SOCK": "資格情報", "ssh_auth_sock": "資格情報", "GNUPGHOME": "資格情報", "KUBECONFIG": "資格情報", "GPG_AGENT_INFO": "資格情報",
		"AWS_REGION": "資格情報", "AZURE_X": "資格情報", "GOOGLE_APPLICATION_CREDENTIALS": "資格情報", "GCP_X": "資格情報", "CLOUDSDK_X": "資格情報",
		"GH_TOKEN": "資格情報", "GITHUB_TOKEN": "資格情報", "ANTHROPIC_API_KEY": "資格情報", "OPENAI_APIKEY": "資格情報", "X_SECRET_Y": "資格情報",
		"DB_PASSWORD": "資格情報", "DB_PASSWD": "資格情報", "MY_CREDENTIALS": "資格情報", "SIGNING_PRIVATE_KEY": "資格情報", "S3_ACCESS_KEY": "資格情報",
	} {
		if err := CheckEnv(k, "v"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckEnv(%q) = %v, want %q を含む error", k, err, want)
		}
	}
	for _, v := range []string{"a\x00b", "a\nb", "a\rb"} {
		if err := CheckEnv("A", v); err == nil {
			t.Errorf("CheckEnv(A, %q) が error にならない", v)
		}
	}
}

// TestCredEnvIsACopy は、CredEnv と HomeSecrets が、写しを返す (呼び手が書き換えても、規則は変わらない) ことを確認する。
func TestCredEnvIsACopy(t *testing.T) {
	keys, prefixes, parts := CredEnv()
	keys[0], prefixes[0], parts[0] = "X", "X", "X"
	if k, p, q := CredEnv(); k[0] == "X" || p[0] == "X" || q[0] == "X" {
		t.Error("CredEnv の写しを書き換えると、規則が変わる")
	}
	h := HomeSecrets()
	h[0] = "X"
	if HomeSecrets()[0] == "X" || !slices.Contains(HomeSecrets(), ".ssh") {
		t.Error("HomeSecrets の写しを書き換えると、規則が変わる")
	}
	if !IsHomeSecret(".claude") || !IsHomeSecret(".gnupg-vault/x") || !IsHomeSecret(".config") || IsHomeSecret(".sshx") || IsHomeSecret("work") {
		t.Error("IsHomeSecret の判定が違う")
	}
}

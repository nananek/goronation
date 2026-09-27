package contract

import (
	"fmt"
	"slices"
	"strings"
)

// 資格情報らしい環境変数の名前 (大文字にして比べる)。credEnvKeys は、名前に credEnvParts を含まないものだけを並べる。
var (
	credEnvKeys     = []string{"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GPG_AGENT_INFO", "GNUPGHOME", "KUBECONFIG"}
	credEnvPrefixes = []string{"AWS_", "AZURE_", "GOOGLE_", "GCP_", "CLOUDSDK_"}
	credEnvParts    = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "PRIVATE_KEY", "ACCESS_KEY"}
)

// CredEnv は、資格情報らしい環境変数の名前の規則 (完全一致・接頭辞・部分一致。大文字にして比べる) の写しを返す。
func CredEnv() (keys, prefixes, parts []string) {
	return slices.Clone(credEnvKeys), slices.Clone(credEnvPrefixes), slices.Clone(credEnvParts)
}

// CheckEnv は、環境変数 1 件を検証する。名前は英数字と _ だけ (= ・NUL・改行を含まない)、値は NUL と改行を含まない。
// 資格情報らしい名前は、呼び手が渡しても error にする。
func CheckEnv(key, value string) error {
	valid := key != ""
	for i, r := range key {
		if !(r == '_' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || i > 0 && '0' <= r && r <= '9') {
			valid = false
		}
	}
	if !valid {
		return fmt.Errorf("Key %q は、英数字と _ だけの名前ではない", key)
	}
	if strings.ContainsAny(value, "\x00\n\r") {
		return fmt.Errorf("Key %q の値が、NUL か改行を含む", key)
	}
	u := strings.ToUpper(key)
	if slices.Contains(credEnvKeys, u) ||
		slices.ContainsFunc(credEnvPrefixes, func(p string) bool { return strings.HasPrefix(u, p) }) ||
		slices.ContainsFunc(credEnvParts, func(p string) bool { return strings.Contains(u, p) }) {
		return fmt.Errorf("Key %q は、資格情報らしい名前 (環境変数で運ばない)", key)
	}
	return nil
}

//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// opencodeFakeHost・opencodeFakeVirtualPort は、opencode.json の baseURL と connectproxy の許可表の
// 両方が使う、仮想のホスト名・ポート (実際にこの名前を解決することは無い。connectproxy が、この
// "host:port" だけを、fake サーバーの実アドレス (127.0.0.1:<動的ポート>) に変換する)。
const (
	opencodeFakeHost        = "fake-openai.test"
	opencodeFakeVirtualPort = 443
)

// opencodeProviderName・opencodeModelName は、opencode.json に書く、fake provider の識別子。
const (
	opencodeProviderName = "fake"
	opencodeModelName    = "fake-model"
)

// opencodeModelRef は、`opencode run -m` に渡す "provider/model" の形。
const opencodeModelRef = opencodeProviderName + "/" + opencodeModelName

// opencodeEnv は、opencode 固有の環境変数: 実際のネットワークに出る不要な通信 (更新確認・モデル
// 一覧・共有・LSP download) を止める。cmd/goronation/agent.go の opencodeProfile と同じ変数だが、
// 別バイナリなので値は独立に持つ (s1-plan の「共有せず独立実装する」方針どおり)。
func opencodeEnv() []bwrap.EnvVar {
	return []bwrap.EnvVar{
		{Key: "OPENCODE_DISABLE_AUTOUPDATE", Value: "1"},
		{Key: "OPENCODE_DISABLE_MODELS_FETCH", Value: "1"},
		{Key: "OPENCODE_DISABLE_SHARE", Value: "1"},
		{Key: "OPENCODE_DISABLE_LSP_DOWNLOAD", Value: "1"},
	}
}

// writeOpencodeConfig は、work/opencode.json を書く。baseURL は、常に
// "https://<opencodeFakeHost>/v1" (仮想ポートは既定の 443 なので URL には出さない): 実際に dial する
// 先は、connectproxy の許可表が決める。
func writeOpencodeConfig(work string) error {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			opencodeProviderName: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "Fake",
				"options": map[string]any{
					"baseURL": fmt.Sprintf("https://%s/v1", opencodeFakeHost),
				},
				"models": map[string]any{
					opencodeModelName: map[string]any{"name": "Fake Model"},
				},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(work, "opencode.json"), b, 0o600)
}

// opencodeArgs は、檻の中で opencode に渡す引数: run --format json -m <provider/model> <message...>。
// session は、複数ターンを採取するとき (PR③) に --session <id> を足すためのもの (空なら付けない)。
func opencodeArgs(session string, message []string) []string {
	args := []string{"run", "--format", "json", "-m", opencodeModelRef}
	if session != "" {
		args = append(args, "--session", session)
	}
	return append(args, message...)
}

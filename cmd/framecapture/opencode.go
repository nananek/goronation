//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nananek/goronation/sandbox/bwrap"
	"github.com/nananek/goronation/tools/fakeproviders/openai"
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

// opencodeArgs は、檻の中で opencode に渡す引数: run --format json -m <provider/model> [--continue] <message...>。
// cont が true なら --continue を足す (同じ HOME の直前のセッションの続きにする。複数ターンの 2 ターン目以降)。
func opencodeArgs(cont bool, message []string) []string {
	args := []string{"run", "--format", "json", "-m", opencodeModelRef}
	if cont {
		args = append(args, "--continue")
	}
	return append(args, message...)
}

// opencodeTitleReply は、opencode が最初のターンで別に投げる「会話のタイトル生成」のリクエストへ返す固定の題。
const opencodeTitleReply = "framecapture"

// maxPeekBody は、タイトル生成のリクエストかを見分けるために読むリクエスト本文の上限 (超えたら、場面の応答に回す)。
const maxPeekBody = 4 << 20

// withTitleRequests は、opencode の最初のターンが本体のリクエストと別に投げる、タイトル生成のリクエスト
// (tools が無く、system メッセージが "You are a title generator" で始まる) を、場面の応答 (scenario の
// Step) を消費させずに固定の題で答える handler にして返す。これが無いと、タイトル生成が Step を 1 つ食い、
// 本体のリクエストの応答が 1 つずれる (順序にも依存する)。タイトル生成でないリクエストは、そのまま next へ渡す。
func withTitleRequests(next http.Handler) http.Handler {
	title := openai.NewServer(openai.Step{Content: opencodeTitleReply})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPeekBody+1))
		r.Body.Close()
		if err != nil || len(body) > maxPeekBody {
			// 読めない・大きすぎる: 場面の応答に回す (本文は、読んだ分だけを渡す)。
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if isTitleRequest(body) {
			title.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isTitleRequest は、body (Chat Completions のリクエスト) が、opencode のタイトル生成かを返す。
func isTitleRequest(body []byte) bool {
	var req struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Tools) != 0 || len(req.Messages) == 0 {
		return false
	}
	first := req.Messages[0]
	text, _ := first.Content.(string)
	return first.Role == "system" && strings.HasPrefix(text, "You are a title generator")
}

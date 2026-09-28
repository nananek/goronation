//go:build linux

package main

import (
	"github.com/nananek/goronation/sandbox/bwrap"
)

// claudeFakeHost は、ANTHROPIC_BASE_URL と connectproxy の許可表の両方が使う、仮想のホスト名
// (opencode.go の opencodeFakeHost と同じ理屈)。実際にこの名前を解決することは無い。
const claudeFakeHost = "fake-anthropic.test"

// claudeFakeVirtualPort は、connectproxy の許可表の key に使う仮想ポート (ANTHROPIC_BASE_URL は
// https のスキームだけで、明示的なポートを書かない。既定の 443 を想定する)。
const claudeFakeVirtualPort = 443

// claudeBaseURL は、檻の中の claude に渡す ANTHROPIC_BASE_URL。実際に dial する先は、
// connectproxy の許可表が決める (claudeFakeHost:claudeFakeVirtualPort → fake サーバーの実アドレス)。
const claudeBaseURL = "https://" + claudeFakeHost

// claudeEnv は、claude 固有の環境変数: ANTHROPIC_BASE_URL (fake サーバーへ向ける)・不要な通信を止める
// 変数 (cmd/goronation/agent.go の claudeProfile と同じ変数だが、別バイナリなので独立に持つ)。
// API キーは、環境変数では渡さない: ANTHROPIC_API_KEY のような「資格情報らしい名前」の環境変数は、
// ダミー値であっても sandbox/bwrap の Argv 検証が拒む (資格情報は環境変数で運ばない、という bwrap 側の
// 一般規則)。claudeArgs の --settings apiKeyHelper で渡す。
func claudeEnv() []bwrap.EnvVar {
	return []bwrap.EnvVar{
		{Key: "ANTHROPIC_BASE_URL", Value: claudeBaseURL},
		{Key: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Value: "1"},
		{Key: "DISABLE_TELEMETRY", Value: "1"},
		{Key: "DISABLE_ERROR_REPORTING", Value: "1"},
		{Key: "DISABLE_AUTOUPDATER", Value: "1"},
		{Key: "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", Value: "1"},
	}
}

// claudeAPIKeyHelperSettings は、--settings に渡す JSON: apiKeyHelper (API キーを標準出力に印字する
// コマンド) に、ダミーのキーを echo するだけのコマンドを指す。fake サーバーは Authorization ヘッダを
// 検査しないので、値そのものに意味は無い (claude 自身が「未ログイン」と判定して起動を拒まないために要る)。
const claudeAPIKeyHelperSettings = `{"apiKeyHelper":"/bin/echo sk-framecapture-dummy"}`

// claudeArgs は、檻の中で claude に渡す引数。message は、標準入力 (stream-json の 1 行) で渡すので、
// 引数には含めない。
func claudeArgs() []string {
	return []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--settings", claudeAPIKeyHelperSettings,
	}
}

// claudeStreamJSONLine は、claude -p --input-format stream-json の標準入力に書く、ユーザーの
// 1 ターン分の JSON (1 行、末尾に改行を付けて渡す)。
func claudeStreamJSONLine(text string) map[string]any {
	return map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	}
}

package egress

// ClaudeHosts は、檻の中の claude が要る宛先で、Config.Allow にそのまま渡せる。呼ぶたびに新しい slice を返す。
// api.anthropic.com は API、platform.claude.com はログインのコード交換とトークンの更新に使う (spike の実測)。
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 で止まる宛先 (raw.githubusercontent.com・Datadog) は、含めない。
// 他の宛先は、使って必要と分かってから足す (拒否は、監査に宛先つきで出る)。
func ClaudeHosts() []string {
	return []string{"api.anthropic.com:443", "platform.claude.com:443"}
}

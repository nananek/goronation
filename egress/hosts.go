package egress

// ClaudeHosts は、檻の中の claude が要る宛先で、Config.Allow にそのまま渡せる。呼ぶたびに新しい slice を返す。
// api.anthropic.com は API、platform.claude.com はログインのコード交換とトークンの更新に使う (spike の実測)。
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 で止まる宛先 (raw.githubusercontent.com・Datadog) は、含めない。
// 他の宛先は、使って必要と分かってから足す (拒否は、監査に宛先つきで出る)。
func ClaudeHosts() []string {
	return []string{"api.anthropic.com:443", "platform.claude.com:443"}
}

// OpenCodeHosts は、檻の中の opencode (OpenCode Zen を使う) が要る宛先で、Config.Allow にそのまま渡せる。呼ぶたびに新しい slice を返す。
// opencode.ai は、Zen のモデル呼び出し (/zen/v1・/zen/go/v1) に使う (spike の実測: 資格情報無しの無料モデルが、この 1 宛先だけで応答した)。
// models.opencode.ai (モデル一覧。同梱の一覧で動く)・api.github.com (更新の確認) は、含めない: OPENCODE_DISABLE_* の 4 つ
// (AUTOUPDATE・MODELS_FETCH・SHARE・LSP_DOWNLOAD) を同時に設定すると、この 2 つの拒否は消えた (変数ごとの切り分けはしていない)。
// 止まらない registry.npmjs.org (プラグインの依存の install。失敗しても動く) と、ripgrep の自動 download の github.com も含めない
// (rg は、ホストの /usr に入れる)。
// 他の provider (api.anthropic.com など) は、使う人が --allow で足す。
func OpenCodeHosts() []string {
	return []string{"opencode.ai:443"}
}

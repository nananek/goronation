//go:build linux

package main

import (
	"strings"

	"github.com/nananek/goronation/egress"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// agentProfile は、goro run が檻の中で動かすエージェント 1 種類ごとの違い。檻の作り (何を bind し、どこを rw にし、通信をどう
// 絞るか) と、セッション・export は、エージェントによらず同じで、ここに無いものは共通。
type agentProfile struct {
	// name は、--agent の値で、実行ファイルを指すオプション (--<name>) の名前。案内にも出す。
	name string
	// exeExample は、スクリプトの実行ファイルを断るときに、指定する実体の例に出す path。
	exeExample string
	// jailExe は、檻の中での実行ファイルの path。
	jailExe string
	// homeName は、状態ディレクトリの下の、檻専用の HOME の名前。エージェントごとに別にする (ログイン状態・会話の履歴を混ぜない)。
	homeName string
	// env は、共通の環境変数 (HOME・PATH・TERM・LANG) に足す、そのエージェントの環境変数 (通信を止めるもの)。
	env []bwrap.EnvVar
	// hosts は、既定の許可宛先。呼ぶたびに新しい slice を返す。
	hosts func() []string
	// loginArgs は、--login のときに、エージェントへ渡す引数 (利用者の引数は、この後ろ)。
	loginArgs []string
	// loginGuide は、--login の起動前に出す案内 (先頭の "goro run: " は、呼び手が付ける)。
	loginGuide string
	// resumeNote は、セッションの案内 (再開・取り出し) の後ろに出す 1 行 (空なら出さない)。
	resumeNote string
	// denyNotes は、拒否されたときに、説明を添える宛先 (と、その説明)。終了後の一覧には出し (隠さない)、説明を添えて、--allow の
	// 例には使わない。許可しなくてよいもの (動作に影響しない) と、許可より先に直すべきものを書く。
	denyNotes map[string]string
}

// exeEnv は、実行ファイルを指す環境変数の名前 (--<name> と同じ効果。テストにも使う)。
func (p agentProfile) exeEnv() string { return "GORO_" + strings.ToUpper(p.name) }

// claudeProfile は、claude (Claude Code) の profile。既定のエージェント。
var claudeProfile = agentProfile{
	name:       "claude",
	exeExample: "/opt/claude-code/bin/claude",
	jailExe:    "/opt/claude/claude",
	homeName:   "home", // 互換: 既存の利用者のログイン状態が、この dir に残っている
	// 通信を止める変数 (許可した宛先以外への、必須でない通信が、拒否として終了後の一覧に出て、必要な宛先に見えるのを避ける):
	// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC・DISABLE_TELEMETRY・DISABLE_ERROR_REPORTING・DISABLE_AUTOUPDATER に加えて、
	// CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL (公式のプラグイン marketplace の自動インストール。これが無いと、
	// 対話起動のたびに downloads.claude.ai:443 と github.com:443 が拒否される。実測: この変数だけで、両方の拒否が消える)。
	env: []bwrap.EnvVar{
		{Key: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Value: "1"},
		{Key: "DISABLE_TELEMETRY", Value: "1"},
		{Key: "DISABLE_ERROR_REPORTING", Value: "1"},
		{Key: "DISABLE_AUTOUPDATER", Value: "1"},
		{Key: "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", Value: "1"},
	},
	hosts: egress.ClaudeHosts,
	// --login も、claude auth login ではなく、素の対話起動にする: 初回の onboarding (テーマ・ログイン・Security notes) を通ると、
	// claude が、認証情報と、onboarding の完了 (.claude.json の hasCompletedOnboarding) を保存する。claude auth login は、
	// 認証情報しか保存せず、次の対話起動が、onboarding (ログイン画面を含む) からやり直しになる。
	loginArgs: nil,
	loginGuide: "ログイン用に claude を対話起動する。テーマを選び、出た URL をホストのブラウザで開いてコードを貼り、" +
		"Security notes で Enter を押したら、/exit で終える (onboarding を最後まで通らないと、次の起動が、ログイン画面からやり直しになる)",
}

// opencodeProfile は、opencode (OpenCode Zen を使う) の profile。実測 (opencode 1.18.32・檻の中) に基づく。
var opencodeProfile = agentProfile{
	name:       "opencode",
	exeExample: "~/.opencode/bin/opencode",
	jailExe:    "/opt/opencode/opencode",
	homeName:   "home-opencode", // claude の HOME (home) とは別: ログイン状態 (auth.json)・会話の履歴 (DB) を混ぜない
	// 通信を止める変数 (実在は、binary の RuntimeFlags で確認):
	//   OPENCODE_DISABLE_AUTOUPDATE   更新の確認 (api.github.com) を止める (実測)
	//   OPENCODE_DISABLE_MODELS_FETCH モデル一覧の取得 (models.opencode.ai) を止める。同梱の一覧で動く (実測: 一覧は、取得を許しても同じ)
	//   OPENCODE_DISABLE_SHARE        会話の共有 (share) を止める。project の設定に share: auto があっても、共有しない
	//   OPENCODE_DISABLE_LSP_DOWNLOAD LSP サーバーの自動 download (github.com) を止める
	// 止められないもの: プラグインの依存の install (registry.npmjs.org。失敗しても動く)。denyNotes に書く。
	env: []bwrap.EnvVar{
		{Key: "OPENCODE_DISABLE_AUTOUPDATE", Value: "1"},
		{Key: "OPENCODE_DISABLE_MODELS_FETCH", Value: "1"},
		{Key: "OPENCODE_DISABLE_SHARE", Value: "1"},
		{Key: "OPENCODE_DISABLE_LSP_DOWNLOAD", Value: "1"},
	},
	hosts: egress.OpenCodeHosts,
	// opencode に onboarding は無い (認証を保存すれば、次の起動は、そのまま使える)。--login は、auth login (provider を選び、
	// Zen なら API キーを貼る) を起動する: 終わると、自分で終了する。
	loginArgs: []string{"auth", "login"},
	loginGuide: "ログイン用に opencode の auth login を起動する。provider を選び (Zen なら OpenCode Zen)、https://opencode.ai/auth で作った " +
		"API キーを貼ると、檻専用の HOME に保存されて終わる (ホストのブラウザの localhost に戻る方式の OAuth は、檻に届かない: API キーの方式を使う)",
	resumeNote: "opencode の会話は、この --agent の檻専用の HOME に残る。続きは、起動後に /sessions で選ぶ。-- --continue は、同じ repo の直近の会話を開く",
	denyNotes: map[string]string{
		"registry.npmjs.org:443": "opencode が、起動のたびに、プラグインの依存 (@opencode-ai/plugin) の install を試す。失敗しても動くので、許可しなくてよい",
		"models.opencode.ai:443": "auth login が、provider の一覧を取ろうとする。同梱の一覧で動くので、許可しなくてよい",
		"github.com:443": "grep ツールが使う ripgrep (rg) を、opencode が download しようとした (PATH に rg が無い)。ホストに rg を入れる " +
			"(Arch: pacman -S ripgrep。檻の /usr に見える)。github.com は許可しないほうがよい",
	},
}

// agents は、--agent に指定できるエージェント。先頭が既定。
var agents = []agentProfile{claudeProfile, opencodeProfile}

// agentByName は、name (--agent の値) のエージェント。無ければ ok が false。
func agentByName(name string) (p agentProfile, ok bool) {
	for _, a := range agents {
		if a.name == name {
			return a, true
		}
	}
	return agentProfile{}, false
}

// agentNames は、--agent に指定できる名前を、"claude か opencode" の形に並べる (エラーの文言用)。
func agentNames() string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.name
	}
	return strings.Join(names, " か ")
}

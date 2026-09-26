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
	// benign は、拒否されても、動作に影響しない宛先と、その説明。終了後の一覧には出すが、説明を添え、--allow の例には使わない。
	benign map[string]string
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

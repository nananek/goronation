//go:build linux

package main

import (
	"path/filepath"
	"strings"

	"github.com/nananek/goronation/egress"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// agentProfile は、goro run が檻の中で動かすエージェント 1 種類ごとの違い。檻の作り (何を bind し、どこを rw にし、通信をどう
// 絞るか) と、セッション・export は、エージェントによらず同じで、ここに無いものは共通。
//
// エージェントごとに違うものは、すべて、この構造体のデータ (と、名前から導出するもの) で、「claude なら A・opencode なら B」の
// 分岐は、コードに書かない。新しいエージェントを足す作業は、profile を 1 つ書いて、agents の表に足すことだけ (宛先は egress の
// 関数、環境変数はこの定数の中)。名前から導出するもの: 環境変数 GORO_<NAME> (exeEnv)・檻の中の path (jailExe)・状態の
// ディレクトリ <state>/agents/<name>/ (dirs)・--agent の値。
type agentProfile struct {
	// name は、--agent の値で、エージェントの識別子: 状態のディレクトリ名・セッションの記録・環境変数の名前 (GORO_<NAME>) の元。
	// [a-z][a-z0-9-]{0,31} (session の記録と同じ形)。表の中で、重ならない。
	name string
	// bin は、PATH で探す実行ファイルの名前。空なら name。
	bin string
	// exeExample は、スクリプトの実行ファイルを断るときに、指定する実体の例に出す path。
	exeExample string
	// env は、共通の環境変数 (HOME・PATH・TERM・LANG) に足す、そのエージェントの環境変数 (通信を止めるもの)。
	env []bwrap.EnvVar
	// hosts は、既定の許可宛先。呼ぶたびに新しい slice を返す。
	hosts func() []string
	// loginArgs は、--login のときに、エージェントへ渡す引数 (利用者の引数は、この後ろ)。
	loginArgs []string
	// loginGuide は、--login の起動前に出す案内 (先頭の "goro run: " は、呼び手が付ける)。ユーザーがすることだけを、短く、命令形で書く
	// (2 行以内)。理由・経緯・制約の説明は、loginUsage (goro run -h) に書く。
	loginGuide string
	// loginUsage は、goro run -h の、このエージェントの --login の説明 (1 行)。理由・制約は、ここに書く。
	loginUsage string
	// exitHint は、goro run -h の、このエージェントの終了操作 (1 語句)。
	exitHint string
	// resumeUsage は、goro run -h の、このエージェントの会話の続きの説明 (空なら出さない)。実行時のメッセージには、出さない。
	resumeUsage string
	// denyNotes は、拒否されたときに、宛先の後ろに添える短い一言 (10 文字前後。原因の推測・経緯は書かない)。終了後の一覧には出し
	// (隠さない)、--allow の例には使わない。許可しなくてよいもの (動作に影響しない) と、許可より先にすることがあるものを書く。
	denyNotes map[string]string
}

// binName は、PATH で探す実行ファイルの名前。
func (p agentProfile) binName() string {
	if p.bin != "" {
		return p.bin
	}
	return p.name
}

// exeEnvName は、名前 name のエージェントの、実行ファイルを指す環境変数の名前 (--bin と同じ効果。--bin > この変数 > PATH): GORO_<NAME 大文字>。
func exeEnvName(name string) string {
	return "GORO_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// exeEnv は、p の、実行ファイルを指す環境変数の名前。
func (p agentProfile) exeEnv() string { return exeEnvName(p.name) }

// jailExe は、檻の中での実行ファイルの path: /opt/<name>/<実行ファイル名>。
func (p agentProfile) jailExe() string { return "/opt/" + p.name + "/" + p.binName() }

// claudeProfile は、claude (Claude Code) の profile。既定のエージェント。
var claudeProfile = agentProfile{
	name:       "claude",
	exeExample: "/opt/claude-code/bin/claude",
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
	loginArgs:  nil,
	loginGuide: "テーマを選び、表示された URL をブラウザで開いて、出たコードを貼ってください。\nSecurity notes で Enter を押し、最後に /exit を入力してください。",
	loginUsage: "対話起動する (初回の onboarding = テーマ・ログイン・Security notes を通す。最後まで通らないと、次の起動がログイン画面からやり直しになる。終わりは /exit)",
	exitHint:   "/exit",
}

// opencodeProfile は、opencode (OpenCode Zen を使う) の profile。実測 (opencode 1.18.32・檻の中) に基づく。
var opencodeProfile = agentProfile{
	name:       "opencode",
	exeExample: "~/.opencode/bin/opencode",
	// 通信を止める変数。名前の実在は、binary の RuntimeFlags で確認した。実測したのは、次の 4 つを同時に設定すると、api.github.com
	// (更新の確認) と models.opencode.ai (モデル一覧) の拒否が消えたこと (変数ごとの切り分けはしていない)。それぞれが止めるはずのものは、
	// 変数の名前と opencode の挙動からの推定で、SHARE と LSP_DOWNLOAD の通信の抑止は、測っていない:
	//   OPENCODE_DISABLE_AUTOUPDATE   更新の確認 (api.github.com)
	//   OPENCODE_DISABLE_MODELS_FETCH モデル一覧の取得 (models.opencode.ai)。同梱の一覧で動く (実測: 一覧は、取得を許しても同じ 77 件)
	//   OPENCODE_DISABLE_SHARE        会話の共有 (share)。project の設定に share: auto があっても、共有しない
	//   OPENCODE_DISABLE_LSP_DOWNLOAD LSP サーバーの自動 download (github.com)
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
	loginArgs:   []string{"auth", "login"},
	loginGuide:  "provider を選び、API キーを貼ってください (キーは https://opencode.ai/auth)。",
	loginUsage:  "auth login を起動する (provider を選び、Zen なら https://opencode.ai/auth のキーを貼る。終わると自分で終了する。ホストのブラウザの localhost に戻る方式の OAuth は、檻に届かないので使えない)",
	exitHint:    "/exit",
	resumeUsage: "会話は、そのエージェント専用の HOME に残る。続きは、起動後に /sessions で選ぶ (-- --continue は、同じ repo の直近の会話を開く)",
	denyNotes: map[string]string{
		"registry.npmjs.org:443": "許可不要",
		"models.opencode.ai:443": "許可不要",
		"github.com:443":         "ホストに rg を入れる (pacman -S ripgrep)",
	},
}

// agentsDirName は、エージェントごとの状態を置く、状態ディレクトリの下のディレクトリの名前。
const agentsDirName = "agents"

// agentDirs は、エージェント 1 種類の状態のディレクトリ (すべて絶対 path)。
type agentDirs struct {
	// home は、檻専用の HOME (ログイン状態・会話の履歴が残る)。エージェントごとに別にする。
	home string
	// loginWork・loginRun は、--login の空の作業ディレクトリ (/work) と run dir (egress の UDS・監査ログ)。エージェントごとに別にする:
	// 共有すると、片方のエージェントの檻が /work に置いた設定 (opencode.json・.claude/settings.json など) が、もう片方の --login の檻
	// (そのエージェントの HOME の認証情報を持つ) で読まれて動く。run dir の監査ログ (過去の拒否宛先) も混ざる。
	loginWork, loginRun string
}

// dirs は、状態ディレクトリ stateDir の下の、p の状態のディレクトリ: <state>/agents/<name>/{home,login-work,login-run}。
// エージェントの状態の置き場は、ここだけ (どのエージェントも、同じ形。名前だけが違う)。セッションは、エージェント共通の
// <state>/sessions/<id> で、そこに作ったエージェントの記録がある。
func (p agentProfile) dirs(stateDir string) agentDirs {
	base := filepath.Join(stateDir, agentsDirName, p.name)
	return agentDirs{home: filepath.Join(base, "home"), loginWork: filepath.Join(base, "login-work"), loginRun: filepath.Join(base, "login-run")}
}

// agents は、--agent に指定できるエージェントの表。先頭が既定。エージェントを足すときは、ここに profile を足す。
var agents = []agentProfile{claudeProfile, opencodeProfile}

// defaultAgent は、--agent を省略したときのエージェント (表の先頭)。
func defaultAgent() agentProfile { return agents[0] }

// legacySessionAgent は、エージェントを記録する前に作ったセッション (agent の記録が無いもの) を作ったエージェント。そのころ、
// 動かせるエージェントは claude だけだった (歴史の事実で、既定の選び方とは別)。
const legacySessionAgent = "claude"

// isDefaultAgent は、name が既定のエージェントか (既定は、案内のコマンドで --agent を省略できる)。
func isDefaultAgent(name string) bool { return name == defaultAgent().name }

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

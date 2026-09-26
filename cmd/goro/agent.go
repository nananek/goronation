//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	// loginUsage は、goro run -h の、このエージェントの --login の説明 (1 行)。理由・制約・API キーの取得 URL は、ここに書く。
	// エージェントの画面の項目名・手順は、書かない (版で変わる。ログインは、エージェント自身の画面で行う)。
	loginUsage string
	// exitHint は、このエージェントの終了操作 (短く)。--login の案内 (loginGuide) と、goro run -h に出す。
	exitHint string
	// resumeUsage は、goro run -h の、このエージェントの会話の続きの説明 (空なら出さない)。実行時のメッセージには、出さない。
	resumeUsage string
	// creds は、認証情報だけを、repo をまたいで共有するための、エージェントごとのデータ (下の credentials)。
	creds credentials
	// seed は、repo ごとの HOME を作るときに置くファイル (エージェントの初回の設定を飛ばすためのもの)。ログイン用の HOME には、置かない
	// (ログインで、初回の設定を、最後まで通す)。認証情報・アカウントの情報は、入れない (下の credentials)。
	seed []homeFile
	// denyNotes は、拒否されたときに、宛先の後ろに添える短い一言 (10 文字前後。原因の推測・経緯は書かない)。終了後の一覧には出し
	// (隠さない)、--allow の例には使わない。許可しなくてよいもの (動作に影響しない) と、許可より先にすることがあるものを書く。
	denyNotes map[string]string
}

// homeFile は、repo ごとの HOME を作るときに置くファイル 1 つ。
type homeFile struct {
	path    string // HOME からの相対 path (クリーンで、HOME の外へ出ない)
	content string
}

// credentials は、認証情報を、repo をまたいで共有する仕組みのデータ。エージェントごとの認証用ディレクトリ (<state>/agents/<name>/auth/) を、
// 全ての檻 (--login も repo も) に jailAuth (rw) で見せる。エージェントに、そこを使わせる方法は、次の 2 つ (両方でもよい):
//
//   - env: エージェントが、認証情報の置き場を変えられるとき、その環境変数 (値は jailAuth)。書き込み・更新のロックも、そこに閉じる。
//   - linkDir・files: 置き場を変えられないとき、HOME の linkDir の下に、files の各名前の symlink (→ jailAuth/<名前>) を作る。エージェントが
//     その場で書くものだけに使える (rename で置き換えると、symlink が、その HOME だけの普通のファイルになり、共有から外れる)。
//
// ホストは、認証用ディレクトリの中身を、読まない・解釈しない (檻が書く敵対入力を、読む場所を作らない)。アカウントの表示情報のように、
// エージェントが、起動のたびに認証情報から作り直すものは、認証情報にも種にも入れない (claude の .claude.json の oauthAccount)。
type credentials struct {
	env     []bwrap.EnvVar
	linkDir string
	files   []string
}

// loginGuide は、--login の起動前に出す案内 (先頭の "goro run: " は、呼び手が付ける)。エージェントの画面の内容 (項目名・手順・URL) は、
// 説明しない: 画面はエージェントの版で変わり、説明は嘘になる (実際に、opencode 2 系で食い違った)。ログインは、エージェント自身の画面で
// 行い、goro は出力を読まない・解釈しない (エージェントの標準入出力は、端末に直結する)。エージェントに共通の 1 行に、終了操作だけを添える。
func (p agentProfile) loginGuide() string {
	return "ログインの画面が出ます。画面の指示に従い、終わったら終了してください (終了: " + p.exitHint + ")。"
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
	// 認証情報 (.credentials.json) は、rename で書き換える (単一ファイルの bind は使えない) が、環境変数で、置き場 (と、更新のロック) だけを
	// 別のディレクトリに移せる (実測: 読み・書き・ロックが、そのディレクトリの中に閉じる)。.claude.json の oauthAccount は、起動のたびに、
	// 認証情報から取り直される (実測: 別のアカウントのものが残る HOME でも、起動で直る) ので、種にも共有にも入れない。
	creds: credentials{env: []bwrap.EnvVar{{Key: "CLAUDE_SECURESTORAGE_CONFIG_DIR", Value: jailAuth}}},
	// 種は、初回の設定 (テーマ・ログイン・Security notes の確認) を、飛ばすためだけ。/work の trust の確認は、repo ごとの初回に出る (意図どおり)。
	seed:       []homeFile{{path: ".claude.json", content: "{\"hasCompletedOnboarding\": true}\n"}},
	loginArgs:  nil,
	loginUsage: "対話起動する (初回の設定とログインは、claude 自身の画面で行う。最後まで通らないと、次の起動がやり直しになる)",
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
	// auth.json (provider の API キー・OAuth) と mcp-auth.json (MCP の認証) は、その場で書く (実測: 同じ inode。symlink を辿って書き、symlink は保たれる)
	// ので、HOME からの symlink で、共有の認証用ディレクトリに向ける。会話の履歴 (opencode.db)・設定・状態は、repo ごとの HOME に残る。
	creds:       credentials{linkDir: ".local/share/opencode", files: []string{"auth.json", "mcp-auth.json"}},
	loginArgs:   []string{"auth", "login"},
	loginUsage:  "auth login を起動する (ログインは、opencode 自身の画面で行う。API キーは https://opencode.ai/auth で作る。ホストのブラウザの localhost に戻る方式の OAuth は、檻に届かないので使えない)",
	exitHint:    "/exit か Ctrl-C",
	resumeUsage: "会話は、repo ごとの HOME に残る (別の repo の会話は見えない)。続きは、起動後に /sessions で選ぶ (-- --continue は、直近の会話を開く)",
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
	// auth は、認証情報の置き場 (ログイン状態)。全ての檻 (--login も repo も) に、jailAuth で rw で見せる。エージェントごとに別で、全 repo で共有する。
	auth string
	// homes は、repo ごとの HOME を置く場所: <homes>/<repo のキー>/ (homeFor)。会話の履歴・メモリ・trust の承認などは、repo ごとに、ここに残る。
	homes string
	// loginHome は、--login の檻の HOME (repo の HOME とは別。種は置かない)。
	loginHome string
	// loginWork・loginRun は、--login の空の作業ディレクトリ (/work) と run dir (egress の UDS・監査ログ)。エージェントごとに別にする:
	// 共有すると、片方のエージェントの檻が /work に置いた設定 (opencode.json・.claude/settings.json など) が、もう片方の --login の檻
	// (そのエージェントの認証情報を持つ) で読まれて動く。run dir の監査ログ (過去の拒否宛先) も混ざる。
	loginWork, loginRun string
}

// homeFor は、repo のキー key の HOME (<homes>/<key>)。key は、session.Store.HomeKey が確かめた形 (16 桁の 16 進) を渡す。
func (d agentDirs) homeFor(key string) string { return filepath.Join(d.homes, key) }

// dirs は、状態ディレクトリ stateDir の下の、p の状態のディレクトリ: <state>/agents/<name>/{auth,homes,login-home,login-work,login-run}。
// エージェントの状態の置き場は、ここだけ (どのエージェントも、同じ形。名前だけが違う)。セッションは、エージェント共通の
// <state>/sessions/<id> で、そこに作ったエージェントと repo のキーの記録がある。
func (p agentProfile) dirs(stateDir string) agentDirs {
	base := filepath.Join(stateDir, agentsDirName, p.name)
	return agentDirs{
		auth: filepath.Join(base, "auth"), homes: filepath.Join(base, "homes"), loginHome: filepath.Join(base, "login-home"),
		loginWork: filepath.Join(base, "login-work"), loginRun: filepath.Join(base, "login-run"),
	}
}

// ensureHome は、HOME ディレクトリ home (0700) を用意する。無ければ、認証情報の symlink (p.creds.linkDir・files) と、seed が true なら
// 種のファイル (p.seed) を置いて作る。ある HOME は、中を見ない・直さない (檻の状態)。
//
// 置く場所は、ホストが作った新しい一時ディレクトリの中だけ (檻が置いたものを辿らない) で、rename で home に据える: 途中で止まっても、
// 種の無い中途半端な HOME が残らない。同じ場所を同時に用意する goro run は、ロックで直列にする (後の方は、先の方の HOME を使う)。
func ensureHome(p agentProfile, home string, seed bool) error {
	parent := filepath.Dir(home)
	if err := ensureDir(parent); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(parent, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // 閉じると、ロックも外れる
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if fi, err := os.Lstat(home); err == nil {
		if !fi.IsDir() { // symlink も、ここで断る (Lstat は、辿らない)
			return fmt.Errorf("%s がディレクトリではない", home)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // rename に成功した後は、もう無い
	var files []homeFile
	if seed {
		files = p.seed
	}
	for _, f := range files {
		if !filepath.IsLocal(f.path) {
			return fmt.Errorf("%s の seed の path %q が、HOME の外を指す", p.name, f.path)
		}
		dst := filepath.Join(tmp, f.path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(f.content), 0o600); err != nil {
			return err
		}
	}
	if len(p.creds.files) > 0 {
		if !filepath.IsLocal(p.creds.linkDir) {
			return fmt.Errorf("%s の認証情報の linkDir %q が、HOME の外を指す", p.name, p.creds.linkDir)
		}
		dir := filepath.Join(tmp, p.creds.linkDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for _, name := range p.creds.files {
			if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
				return fmt.Errorf("%s の認証情報のファイル名 %q が、名前 1 つでない", p.name, name)
			}
			if err := os.Symlink(jailAuth+"/"+name, filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp, home)
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

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// 結合テスト (bwrap が要る): --agent opencode。偽の opencode は、偽の claude と同じテストバイナリ (argv[0] = opencode で再実行される。
// GORO_OPENCODE で指す)。檻の中の path・専用の HOME・環境変数・許可宛先が、opencode のものになり、claude の既定が変わらないことを確かめる。

// agentPath は、エージェント name の状態ディレクトリ (<state>/agents/<name>) の下の path (配置は、ここにリテラルで書いて固定する)。
func (f *runFixture) agentPath(name string, elem ...string) string {
	return filepath.Join(append([]string{f.stateDir(), "agents", name}, elem...)...)
}

// opencode の檻: 実行ファイル (ホストと同じ path) が見え (claude の実行ファイルは無い)、専用の HOME (<state>/agents/opencode/home) が rw、
// ホストの opencode の認証情報・設定・状態ディレクトリの他のものは見えない。環境変数は、共通の 4 つ + OPENCODE_DISABLE_* だけ。
func TestRunOpenCodeCage(t *testing.T) {
	f := newRunFixture(t)
	hostAuth := filepath.Join(f.home, ".local", "share", "opencode", "auth.json")
	hostConf := filepath.Join(f.home, ".config", "opencode", "opencode.json")
	id, clone, run := f.newSession(t, "--agent", "opencode")
	home, ocExe, goroExe, claudeExe := f.agentPath("opencode", "home"), f.binPath("opencode"), f.binPath("goro"), f.binPath("claude")
	probes := []string{
		"stat:" + ocExe, "stat:" + claudeExe, "stat:" + goroExe, "dial:127.0.0.1:3128",
		"stat:" + hostAuth, "stat:" + filepath.Join(f.home, ".local", "share", "opencode"), "stat:" + hostConf,
		"stat:" + filepath.Join(f.home, ".ssh", "id_test"), "stat:" + f.repo, "stat:" + f.agentPath("claude"),
		"write:/usr/x", "write:" + run + "/x",
		"write:" + home + "/x", "write:" + clone + "/x", "write:/tmp/x",
		"mnt:" + ocExe, "mnt:" + home, "mnt:" + clone,
		"list:" + f.home, "list:" + filepath.Join(f.stateDir(), "agents"), "list:" + f.bin,
	}
	r := f.goro(t, append([]string{"run", "--agent", "opencode", "--session", id, "--", "probe"}, probes...)...).mustOK(t)
	_, res := parseOut(r.stdout)
	for op, want := range map[string]string{
		"stat:" + ocExe: "ok", "stat:" + goroExe: "ok", "dial:127.0.0.1:3128": "ok",
		"write:" + home + "/x": "ok", "write:" + clone + "/x": "ok", "write:/tmp/x": "ok",
		"mnt:" + ocExe: "ro", "mnt:" + home: "rw", "mnt:" + clone: "rw",
		"list:" + f.home: "ok:.local", "list:" + filepath.Join(f.stateDir(), "agents"): "ok:opencode", "list:" + f.bin: "ok:goro,opencode",
	} {
		if res[op] != want {
			t.Errorf("%s = %q, want %q", op, res[op], want)
		}
	}
	for _, op := range []string{
		"stat:" + claudeExe, "stat:" + hostAuth, "stat:" + filepath.Join(f.home, ".local", "share", "opencode"), "stat:" + hostConf,
		"stat:" + filepath.Join(f.home, ".ssh", "id_test"), "stat:" + f.repo, "stat:" + f.agentPath("claude"),
		"write:/usr/x", "write:" + run + "/x",
	} {
		if !strings.HasPrefix(res[op], "err") {
			t.Errorf("%s = %q, want err (見えない・書けない)", op, res[op])
		}
	}
	for _, secret := range []string{"PRIVATE-KEY-MARKER", "TOKEN-MARKER", "OPENCODE-AUTH-MARKER", "OPENCODE-CONFIG-MARKER"} {
		if strings.Contains(r.stdout, secret) || strings.Contains(r.stderr, secret) {
			t.Errorf("出力に、ホストの秘密の中身 %s がある", secret)
		}
	}

	// 環境変数と、起動の状態: 許可リストだけ。claude の変数は無く、opencode の 4 つがある。
	info := f.goro(t, "run", "--agent", "opencode", "--session", id, "--", "info").mustOK(t)
	kv, _ := parseOut(info.stdout)
	if kv["home"] != home || kv["cwd"] != clone || kv["https_proxy"] != "http://127.0.0.1:3128" || kv["args"] != `["info"]` {
		t.Errorf("HOME・cwd・HTTPS_PROXY・args = %q・%q・%q・%q, want %q・%q", kv["home"], kv["cwd"], kv["https_proxy"], kv["args"], home, clone)
	}
	allowed := map[string]bool{}
	for _, n := range []string{"HOME", "PATH", "TERM", "LANG", "OPENCODE_DISABLE_AUTOUPDATE", "OPENCODE_DISABLE_MODELS_FETCH",
		"OPENCODE_DISABLE_SHARE", "OPENCODE_DISABLE_LSP_DOWNLOAD", "HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy", "PWD"} {
		allowed[n] = true
	}
	names := strings.Split(kv["env"], ",")
	for _, n := range names {
		if !allowed[n] {
			t.Errorf("檻の環境変数に、許可リスト外の %s がある (%s)", n, kv["env"])
		}
	}
	for n := range allowed {
		if n != "PWD" && !strings.Contains(","+kv["env"]+",", ","+n+",") {
			t.Errorf("檻の環境変数に %s が無い (%s)", n, kv["env"])
		}
	}
}

// エージェントごとに、檻専用の HOME は別: opencode の起動は agents/opencode/ だけを作り、claude の状態 (agents/claude/) に触れない。
// --login は、opencode では auth login の引数で起動する。ログイン状態は、agents/opencode/home に残り、次の opencode の檻に見え、claude の檻には見えない。
func TestRunOpenCodeLoginAndSeparateHome(t *testing.T) {
	f := newRunFixture(t)
	r := f.goro(t, "run", "--agent", "opencode", "--login", "--", "--extra").mustOK(t)
	kv, _ := parseOut(r.stdout)
	if kv["args"] != `["auth" "login" "--extra"]` {
		t.Errorf("opencode の引数 = %s, want [auth login --extra]", kv["args"])
	}
	if kv["cwd"] != f.agentPath("opencode", "login-work") || kv["work"] != "" || kv["home"] != f.agentPath("opencode", "home") {
		t.Errorf("cwd・cwd の中身・HOME = %q・%q・%q (空の作業ディレクトリ <state>/agents/opencode/login-work のはず)", kv["cwd"], kv["work"], kv["home"])
	}
	if b, err := os.ReadFile(filepath.Join(f.agentPath("opencode", "home"), "login-marker")); err != nil || string(b) != "logged-in\n" {
		t.Errorf("ログイン状態が、opencode 専用の HOME (<state>/agents/opencode/home) に残っていない: %q, %v", b, err)
	}
	if _, err := os.Lstat(f.agentPath("claude", "home")); err == nil {
		t.Error("opencode の起動が、claude の状態 (<state>/agents/claude) を作った")
	}
	if fi, err := os.Stat(f.agentPath("opencode", "home")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("opencode 専用の HOME の権限 = %v, %v, want 0700", fi, err)
	}
	for _, want := range []string{"ログインの画面が出ます。画面の指示に従い、終わったら終了してください (終了: /exit か Ctrl-C)。", "ログイン状態: " + f.agentPath("opencode", "home"), "goro run --agent opencode --repo PATH"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("--login の案内に %q が無い:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stderr, "Security notes") || strings.Contains(r.stderr, "provider") || strings.Contains(r.stderr, "セッション:") {
		t.Errorf("--login の案内に、claude の案内が混ざっている:\n%s", r.stderr)
	}

	// 次の opencode の檻に、ログイン状態が見える。claude の檻には、見えない (別の HOME)。
	next := f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--", "marker").mustOK(t)
	if !strings.Contains(next.stdout, `marker="logged-in\n"`) {
		t.Errorf("ログイン状態が、次の opencode の檻に見えない:\n%s", next)
	}
	other := f.goro(t, "run", "--repo", f.repo, "--", "marker").mustOK(t)
	if strings.Contains(other.stdout, "logged-in") || !strings.Contains(other.stdout, `marker=""`) {
		t.Errorf("opencode のログイン状態が、claude の檻に見える:\n%s", other)
	}
	if _, err := os.Stat(f.agentPath("claude", "home")); err != nil {
		t.Errorf("claude の起動が、claude の HOME (<state>/agents/claude/home) を作っていない: %v", err)
	}
}

// 既定の許可宛先は、エージェントごと: opencode は opencode.ai だけ (claude の宛先は、opencode の檻では拒否される)。
// --allow で足した宛先は、通る。拒否は、監査ログと、終了後の案内に出る (許可不要ものは、説明つき)。
func TestRunOpenCodeEgress(t *testing.T) {
	f := newRunFixture(t)
	r := f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--allow", "example.org:443", "--",
		"connect", "opencode.ai:443", "example.org:443", "api.anthropic.com:443", "platform.claude.com:443", "registry.npmjs.org:443", "models.opencode.ai:443",
		"github.com:443").mustOK(t)
	_, res := parseOut(r.stdout)
	for _, target := range []string{"opencode.ai:443", "example.org:443"} {
		if s := res[target]; s == "403" || strings.HasPrefix(s, "err") || s == "" {
			t.Errorf("許可した %s = %q, want 403 でも err でもない (200・502・504)", target, s)
		}
	}
	for _, target := range []string{"api.anthropic.com:443", "platform.claude.com:443", "registry.npmjs.org:443", "models.opencode.ai:443", "github.com:443"} {
		if res[target] != "403" {
			t.Errorf("opencode の檻で、許可外の %s = %q, want 403", target, res[target])
		}
	}
	for _, want := range []string{"拒否された宛先", "registry.npmjs.org:443 (1 回)", "models.opencode.ai:443 (1 回)", "許可不要",
		"github.com:443 (1 回)", "pacman -S ripgrep", "api.anthropic.com:443 (1 回)", "--allow api.anthropic.com:443 を付けて"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("終了後の案内に %q が無い:\n%s", want, r.stderr)
		}
	}
	if strings.Count(r.stderr, "opencode.ai:443 (") != 1 { // models.opencode.ai:443 (1 回) だけ (許可した opencode.ai:443 自体は、出ない)
		t.Errorf("許可した opencode.ai:443 が、拒否の一覧に出ている:\n%s", r.stderr)
	}

	// claude の既定 (--agent なし) は、これまで通り: claude の宛先が通り、opencode.ai は拒否される。
	c := f.goro(t, "run", "--repo", f.repo, "--", "connect", "api.anthropic.com:443", "platform.claude.com:443", "opencode.ai:443").mustOK(t)
	_, res = parseOut(c.stdout)
	for _, target := range []string{"api.anthropic.com:443", "platform.claude.com:443"} {
		if s := res[target]; s == "403" || strings.HasPrefix(s, "err") || s == "" {
			t.Errorf("claude の既定で、許可した %s = %q", target, s)
		}
	}
	if res["opencode.ai:443"] != "403" {
		t.Errorf("claude の檻で、opencode.ai:443 = %q, want 403", res["opencode.ai:443"])
	}
	if strings.Contains(c.stderr, "許可不要") {
		t.Errorf("claude の案内に、opencode の説明が出た:\n%s", c.stderr)
	}
}

// opencode が作ったセッションは、opencode で再開できる (同じ clone が見える)。--agent は省略できる。案内の再開のコマンドには、
// --agent opencode が付く。claude が作ったセッションを opencode で使う場合は、TestRunSessionKeepsItsAgent。
func TestRunOpenCodeResumesSession(t *testing.T) {
	f := newRunFixture(t)
	run1 := f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--", "commit", "hello.txt", "hi there", "add hello").mustOK(t)
	id := sessionID(t, run1)
	if !strings.Contains(run1.stderr, "goro run --agent opencode --session "+id+"\n") {
		t.Errorf("opencode の新しいセッションの案内:\n%s", run1.stderr)
	}
	for _, args := range [][]string{
		{"run", "--agent", "opencode", "--session", id, "--", "gitlog", "hello.txt"},
		{"run", "--session", id, "--", "gitlog", "hello.txt"}, // --agent を省略: 記録のエージェントで動く
	} {
		run2 := f.goro(t, args...).mustOK(t)
		if !strings.Contains(run2.stdout, `file="hi there"`) {
			t.Errorf("%v: 再開した clone に、前の commit が見えない:\n%s", args, run2)
		}
		if !strings.Contains(run2.stderr, "goro run --agent opencode --session "+id+"\n") ||
			!strings.Contains(run2.stderr, "goro export "+id+"\n") || strings.Contains(run2.stderr, "/sessions") { // 会話の続きの説明は、-h だけ
			t.Errorf("%v: opencode の終了後の案内:\n%s", args, run2.stderr)
		}
	}
}

// セッションは、作ったエージェントで動かす。clone には、エージェントが置いた設定 (.claude/settings.json・opencode.json など) が
// 残り、次にそこで動くエージェントが読んで実行する: 別のエージェント (別の HOME の認証情報と許可宛先を持つ) で使い回さない。
//   - 作ったエージェントは、セッションのディレクトリに記録される (檻に見えない場所)。
//   - --session で --agent を省略すると、記録のエージェントで動く (HOME・環境変数・許可宛先も、そのエージェントのもの)。
//   - 記録と違う --agent は、檻を起動せず (HOME も作らず)、原因の分かるエラーで断る。どちらの向きも。
//   - 記録の無い (エージェントを記録する前の) セッションは、claude。記録が壊れていれば、断る。
func TestRunSessionKeepsItsAgent(t *testing.T) {
	f := newRunFixture(t)
	cage := func(t *testing.T, args ...string) (kv map[string]string, r goroResult) {
		t.Helper()
		r = f.goro(t, args...).mustOK(t)
		kv, _ = parseOut(r.stdout)
		return kv, r
	}
	hasEnv := func(kv map[string]string, name string) bool { return strings.Contains(","+kv["env"]+",", ","+name+",") }
	refused := func(t *testing.T, r goroResult, want ...string) {
		t.Helper()
		if r.code != 1 || r.stdout != "" || strings.Contains(r.stderr, "再開:") {
			t.Errorf("断られていない (終了コード 1・檻を起動しない・案内なし のはず):\n%s", r)
		}
		for _, w := range want {
			if !strings.Contains(r.stderr, w) {
				t.Errorf("エラーに %q が無い:\n%s", w, r.stderr)
			}
		}
	}
	agentFile := func(id string) string { return filepath.Join(f.stateDir(), "sessions", id, "agent") }

	// claude が作ったセッション (--agent を省略)。記録は、セッションのディレクトリの直下にあり、clone・run の中には無い。
	cid := sessionID(t, f.goro(t, "run", "--repo", f.repo, "--", "exit", "0").mustOK(t))
	if b, err := os.ReadFile(agentFile(cid)); err != nil || string(b) != "claude\n" {
		t.Fatalf("claude のセッションの記録 = %q, %v", b, err)
	}
	// opencode でその再開を頼むと、断る。opencode の HOME も作らない (檻を起動する前に断る)。
	r := f.goro(t, "run", "--agent", "opencode", "--session", cid, "--", "info")
	refused(t, r, "このセッションは claude で作った", "--agent opencode では使えない", "新しく作る: goro run --agent")
	if _, err := os.Lstat(f.agentPath("opencode", "home")); err == nil {
		t.Error("断ったのに、opencode の HOME が作られた")
	}

	// opencode が作ったセッション。
	oid := sessionID(t, f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--", "exit", "0").mustOK(t))
	if b, err := os.ReadFile(agentFile(oid)); err != nil || string(b) != "opencode\n" {
		t.Fatalf("opencode のセッションの記録 = %q, %v", b, err)
	}
	refused(t, f.goro(t, "run", "--agent", "claude", "--session", oid, "--", "info"), "このセッションは opencode で作った", "--agent claude では使えない")

	// --agent の省略・記録と同じ --agent: 記録のエージェントで動く。
	for _, args := range [][]string{{"--session", cid}, {"--agent", "claude", "--session", cid}} {
		kv, r := cage(t, append(append([]string{"run"}, args...), "--", "info")...)
		if !hasEnv(kv, "DISABLE_TELEMETRY") || hasEnv(kv, "OPENCODE_DISABLE_AUTOUPDATE") || strings.Contains(r.stderr, "--agent opencode") {
			t.Errorf("%v: claude の檻になっていない (env=%s):\n%s", args, kv["env"], r.stderr)
		}
	}
	for _, args := range [][]string{{"--session", oid}, {"--agent", "opencode", "--session", oid}} {
		kv, r := cage(t, append(append([]string{"run"}, args...), "--", "info")...)
		if !hasEnv(kv, "OPENCODE_DISABLE_AUTOUPDATE") || hasEnv(kv, "DISABLE_TELEMETRY") || !strings.Contains(r.stderr, "goro run --agent opencode --session "+oid) {
			t.Errorf("%v: opencode の檻になっていない (env=%s):\n%s", args, kv["env"], r.stderr)
		}
	}
	// 省略時の許可宛先も、記録のエージェントのもの。
	_, conn := cage(t, "run", "--session", oid, "--", "connect", "opencode.ai:443", "api.anthropic.com:443")
	_, res := parseOut(conn.stdout)
	if res["opencode.ai:443"] == "403" || res["api.anthropic.com:443"] != "403" {
		t.Errorf("--agent を省略した opencode のセッションの許可宛先: %v", res)
	}

	// 一覧: エージェントの名前が出る。
	ss := f.goro(t, "sessions").mustOK(t)
	for id, want := range map[string]string{cid: "  claude  repo", oid: "  opencode  repo"} {
		if line := lineWith(ss.stdout, id); !strings.HasSuffix(line, want) {
			t.Errorf("sessions の %s の行 = %q, want 末尾 %q", id, line, want)
		}
	}

	// 記録の無い (エージェントを記録する前の) セッションは、claude。省略でも、--agent claude でも動き、opencode は断る。
	if err := os.Remove(agentFile(cid)); err != nil {
		t.Fatal(err)
	}
	if kv, _ := cage(t, "run", "--session", cid, "--", "info"); !hasEnv(kv, "DISABLE_TELEMETRY") {
		t.Errorf("記録の無いセッションが、claude で動かない (env=%s)", kv["env"])
	}
	refused(t, f.goro(t, "run", "--agent", "opencode", "--session", cid, "--", "info"), "このセッションは claude で作った")
	if line := lineWith(f.goro(t, "sessions").mustOK(t).stdout, cid); !strings.HasSuffix(line, "  claude  repo") {
		t.Errorf("記録の無いセッションの一覧の行 = %q, want claude", line)
	}

	// 記録が壊れている (形が違う・通常のファイルでない) セッションは、断る (黙って claude や opencode にしない)。一覧は ? を出す。
	for name, mk := range map[string]func() error{
		"形が違う": func() error { return os.WriteFile(agentFile(cid), []byte("Not An Agent\n"), 0o600) },
		"symlink": func() error {
			os.Remove(agentFile(cid))
			return os.Symlink(agentFile(oid), agentFile(cid)) // 先は、形の正しい opencode の記録。読まれたら opencode で動いてしまう
		},
	} {
		os.Remove(agentFile(cid))
		if err := mk(); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"--session", cid}, {"--agent", "claude", "--session", cid}, {"--agent", "opencode", "--session", cid}} {
			refused(t, f.goro(t, append(append([]string{"run"}, args...), "--", "info")...), "セッションを使えない", "エージェントの記録")
		}
		if line := lineWith(f.goro(t, "sessions").mustOK(t).stdout, cid); !strings.HasSuffix(line, "  ?  repo") {
			t.Errorf("%s: 壊れた記録の一覧の行 = %q, want ?", name, line)
		}
	}
}

// lineWith は、out の行のうち、substr を含む最初の行 (無ければ空)。
func lineWith(out, substr string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, substr) {
			return l
		}
	}
	return ""
}

// --login の作業ディレクトリ (<state>/agents/<名前>/login-work) と run dir は、エージェントごとに別: 片方の --login の檻が作業ディレクトリに置いたものが、
// もう片方の --login の檻の作業ディレクトリに見えてはいけない (どちらの向きも)。run dir の egress.log (過去の拒否宛先) も、別。claude の名前は、そのまま。
func TestRunLoginDirsSeparateBothWays(t *testing.T) {
	f := newRunFixture(t)
	// opencode の --login の檻が、claude の設定を置く。
	f.goro(t, "run", "--agent", "opencode", "--login", "--", "commit", "settings.json", "PLANTED-BY-OPENCODE-LOGIN", "msg")
	if b, err := os.ReadFile(f.agentPath("opencode", "login-work", "settings.json")); err != nil || string(b) != "PLANTED-BY-OPENCODE-LOGIN" {
		t.Fatalf("前提: opencode の --login の檻が、作業ディレクトリ (<state>/agents/opencode/login-work) に書けていない: %q, %v", b, err)
	}
	r := f.goro(t, "run", "--login").mustOK(t)
	if kv, _ := parseOut(r.stdout); kv["work"] != "" {
		t.Errorf("claude の --login の檻の作業ディレクトリに、opencode の --login の檻が置いたものが見える: work=%q\n%s", kv["work"], r)
	}
	// 拒否された宛先の履歴 (egress.log) も、別。
	f.goro(t, "run", "--agent", "opencode", "--login", "--", "connect", "only-opencode.example:443").mustOK(t)
	f.goro(t, "run", "--login", "--", "connect", "only-claude.example:443").mustOK(t)
	for dir, want := range map[string]string{f.agentPath("claude", "login-run"): "only-claude.example", f.agentPath("opencode", "login-run"): "only-opencode.example"} {
		b, err := os.ReadFile(filepath.Join(dir, egressLogName))
		other := "only-opencode.example"
		if want == other {
			other = "only-claude.example"
		}
		if err != nil || !strings.Contains(string(b), want) || strings.Contains(string(b), other) {
			t.Errorf("%s/egress.log に、%s だけがあるはず (%s は無い): %q, %v", dir, want, other, b, err)
		}
	}
}

// opencode がスクリプト (npm のラッパーなど) なら、檻を起こす前に断る (セッションも作らない)。--bin で指しても、同じ。
// 廃止した --claude は、代わりを教えて断る。未知のエージェントも断る。
func TestRunOpenCodeRejectsScriptAndWrongFlag(t *testing.T) {
	f := newRunFixture(t)
	script := filepath.Join(f.dir, "opencode-wrapper")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env node\nconsole.log('x')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--bin", script)
	if r.code != 1 || !strings.Contains(r.stderr, "スクリプト") || !strings.Contains(r.stderr, "--bin PATH か GORO_OPENCODE") {
		t.Errorf("スクリプトの opencode:\n%s", r)
	}
	if ss := f.goro(t, "sessions").mustOK(t); !strings.Contains(ss.stdout, "セッションは無い") {
		t.Errorf("断ったのに、セッションが作られた:\n%s", ss.stdout)
	}
	r = f.goro(t, "run", "--repo", f.repo, "--claude", f.exe)
	if r.code != exitUsage || !strings.Contains(r.stderr, "廃止した。--bin PATH を使う") {
		t.Errorf("廃止した --claude:\n%s", r)
	}
	r = f.goro(t, "run", "--agent", "codex", "--repo", f.repo)
	if r.code != exitUsage || !strings.Contains(r.stderr, "claude か opencode") {
		t.Errorf("未知のエージェント:\n%s", r)
	}
	// 環境変数 GORO_OPENCODE も効く (fixture の値は、テストバイナリ。同じ名前の最後の値が使われる)。
	f.env = append(f.env, "GORO_OPENCODE="+script)
	r = f.goro(t, "run", "--agent", "opencode", "--repo", f.repo)
	if r.code != 1 || !strings.Contains(r.stderr, "スクリプト") {
		t.Errorf("環境変数 GORO_OPENCODE のスクリプト:\n%s", r)
	}
}

// エージェントを足す作業は、profile を 1 つ表に足すことだけ (goro の子プロセスにだけ、テスト用の第 3 の profile fakeagent を足してある。
// fake の側の名前の登録は、テストの道具)。それだけで、--agent fakeagent が通り、状態が <state>/agents/fakeagent/ に作られ、
// 環境変数・許可宛先・--login・セッションの記録・案内・usage が、そのエージェントのものになる。他のエージェントの状態には、触れない。
func TestRunThirdAgent(t *testing.T) {
	f := newRunFixture(t)
	f.env = append(f.env, "GORO_FAKEAGENT="+f.binPath("fakeagent"))
	envHas := func(kv map[string]string, name string) bool { return strings.Contains(","+kv["env"]+",", ","+name+",") }

	// 起動: 環境変数は、共通 + fakeagent の profile のもの。檻の中の path は、ホストと同じ (実行ファイルは ro で見える。claude のは、見えない)。
	fake, claudeExe := f.binPath("fakeagent"), f.binPath("claude")
	r := f.goro(t, "run", "--agent", "fakeagent", "--repo", f.repo, "--", "probe", "stat:"+fake, "stat:"+claudeExe, "mnt:"+fake).mustOK(t)
	_, res := parseOut(r.stdout)
	if res["stat:"+fake] != "ok" || !strings.HasPrefix(res["stat:"+claudeExe], "err") || res["mnt:"+fake] != "ro" {
		t.Errorf("檻の中の実行ファイルの path: %v", res)
	}
	id := sessionID(t, r)
	info := f.goro(t, "run", "--session", id, "--", "info").mustOK(t) // --agent を省略: 記録のエージェント
	kv, _ := parseOut(info.stdout)
	if !envHas(kv, "FAKEAGENT_MODE") || envHas(kv, "DISABLE_TELEMETRY") || envHas(kv, "OPENCODE_DISABLE_AUTOUPDATE") || kv["home"] != f.agentPath("fakeagent", "home") {
		t.Errorf("fakeagent の環境変数 = %s", kv["env"])
	}
	// 状態: <state>/agents/fakeagent/ だけ。他のエージェントの状態は、作らない・触れない。
	if fi, err := os.Stat(f.agentPath("fakeagent", "home")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("fakeagent の HOME: %v, %v", fi, err)
	}
	ents, err := os.ReadDir(filepath.Join(f.stateDir(), "agents"))
	if err != nil || len(ents) != 1 || ents[0].Name() != "fakeagent" {
		t.Errorf("agents/ の中 = %v, %v (fakeagent だけのはず)", ents, err)
	}
	// セッションの記録・一覧・案内。
	if b, err := os.ReadFile(filepath.Join(f.stateDir(), "sessions", id, "agent")); err != nil || string(b) != "fakeagent\n" {
		t.Errorf("セッションの記録 = %q, %v", b, err)
	}
	if line := lineWith(f.goro(t, "sessions").mustOK(t).stdout, id); !strings.HasSuffix(line, "  fakeagent  repo") {
		t.Errorf("sessions の行 = %q", line)
	}
	for _, want := range []string{"goro run --agent fakeagent --session " + id + "\n"} {
		if !strings.Contains(info.stderr, want) {
			t.Errorf("終了後の案内に %q が無い:\n%s", want, info.stderr)
		}
	}
	// 記録と違う --agent は断る (別のエージェントでは、使えない)。
	if r := f.goro(t, "run", "--agent", "claude", "--session", id, "--", "info"); r.code != 1 || !strings.Contains(r.stderr, "このセッションは fakeagent で作った") {
		t.Errorf("別のエージェントでの再開:\n%s", r)
	}

	// 許可宛先: profile の hosts だけ (他のエージェントの宛先は、通らない)。
	conn := f.goro(t, "run", "--agent", "fakeagent", "--repo", f.repo, "--", "connect", "fake.example:443", "api.anthropic.com:443", "opencode.ai:443").mustOK(t)
	_, res = parseOut(conn.stdout)
	if s := res["fake.example:443"]; s == "403" || strings.HasPrefix(s, "err") || s == "" {
		t.Errorf("fakeagent の許可宛先 fake.example:443 = %q", s)
	}
	if res["api.anthropic.com:443"] != "403" || res["opencode.ai:443"] != "403" {
		t.Errorf("他のエージェントの宛先が通る: %v", res)
	}

	// --login: profile の loginArgs で起動し、状態は agents/fakeagent/{home,login-work,login-run}。案内は profile のもの。
	login := f.goro(t, "run", "--agent", "fakeagent", "--login", "--", "commit", "x.txt", "PLANT", "msg")
	if !strings.Contains(login.stderr, "ログインの画面が出ます。画面の指示に従い、終わったら終了してください (終了: /quit)。") || !strings.Contains(login.stderr, "goro run --agent fakeagent --repo PATH") ||
		!strings.Contains(login.stderr, "ログイン状態: "+f.agentPath("fakeagent", "home")) {
		t.Errorf("--login の案内:\n%s", login.stderr)
	}
	for _, p := range []string{f.agentPath("fakeagent", "home", "login-marker"), f.agentPath("fakeagent", "login-work", "x.txt"), f.agentPath("fakeagent", "login-run", egressLogName)} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("--login の状態が、%s に無い: %v", p, err)
		}
	}

	// スクリプトの拒否・--bin・環境変数の名前は、profile から: GORO_FAKEAGENT。
	script := filepath.Join(f.dir, "fakeagent-wrapper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := f.goro(t, "run", "--agent", "fakeagent", "--repo", f.repo, "--bin", script); r.code != 1 || !strings.Contains(r.stderr, "--bin PATH か GORO_FAKEAGENT") || !strings.Contains(r.stderr, "/opt/fakeagent/bin/fakeagent") {
		t.Errorf("スクリプトの fakeagent:\n%s", r)
	}

	// goro run -h の usage (子プロセスの goro = 表に第 3 の profile がある) に、profile の内容が出る。
	h := f.goro(t, "run", "-h")
	for _, want := range []string{"fakeagent", "GORO_FAKEAGENT", "fake.example:443", "login を起動する (テスト用の説明。この文が -h に出る)", "fakeagent の続きの説明 (テスト用。-h だけに出る)", "/quit", "claude (既定)"} {
		if !strings.Contains(h.stderr, want) {
			t.Errorf("-h に %q が無い:\n%s", want, h.stderr)
		}
	}
	if _, err := os.Lstat(f.agentPath("claude")); err == nil {
		t.Error("fakeagent の起動が、claude の状態 (agents/claude) を作った")
	}
	if _, err := os.Lstat(f.agentPath("opencode")); err == nil {
		t.Error("fakeagent の起動が、opencode の状態 (agents/opencode) を作った")
	}
}

// 檻の中のプロセスが、同じ檻の中の別のプロセス (opencode 2 系の background service など) へ、http://127.0.0.1:PORT で繋ぐとき、その通信は、
// proxy (egress) を通らず、直接届く。NO_PROXY が無いと、その通信が egress に届いて、拒否される (reason=method・405)。
// 檻の境界は変わらない: 直接届くのは、檻の中の loopback だけで、ホストの loopback には、NO_PROXY があっても届かない (ネットワークが無い)。
func TestRunLoopbackBypassesProxy(t *testing.T) {
	f := newRunFixture(t)
	hostL, err := net.Listen("tcp", "127.0.0.1:0") // ホストの loopback で待ち受ける (檻の loopback とは別)
	if err != nil {
		t.Fatal(err)
	}
	defer hostL.Close()
	var hostAccepts atomic.Int32
	go func() {
		for {
			c, err := hostL.Accept()
			if err != nil {
				return
			}
			hostAccepts.Add(1)
			c.Close()
		}
	}()

	for _, agent := range [][]string{{}, {"--agent", "opencode"}} {
		args := append(append([]string{"run"}, agent...), "--repo", f.repo, "--", "loopback")
		r := f.goro(t, args...).mustOK(t)
		_, res := parseOut(r.stdout)
		if res["loopback"] != "200 direct" {
			t.Errorf("%v: 檻の中の loopback への GET = %q, want 200 direct (proxy を通らない)\n%s", agent, res["loopback"], r)
		}
		id := sessionID(t, r)
		log, err := os.ReadFile(filepath.Join(f.stateDir(), "sessions", id, "run", egressLogName))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(log), "127.0.0.1") || strings.Contains(r.stderr, "127.0.0.1") {
			t.Errorf("%v: 檻の中の loopback への通信が、egress に届いている:\n%s\n%s", agent, log, r.stderr)
		}

		// ホストの loopback には、届かない (NO_PROXY で直接に行っても、檻の loopback には、そのポートの待ち受けが無い)。
		args = append(append([]string{"run"}, agent...), "--repo", f.repo, "--", "loopback", hostL.Addr().String())
		r = f.goro(t, args...).mustOK(t)
		_, res = parseOut(r.stdout)
		if !strings.HasPrefix(res["loopback"], "err") {
			t.Errorf("%v: ホストの loopback %s に届いた: %q", agent, hostL.Addr(), res["loopback"])
		}
	}
	if n := hostAccepts.Load(); n != 0 {
		t.Errorf("ホストの loopback が、檻からの接続を %d 回受けた", n)
	}
}

// goro は、エージェントの出力を読まず、解釈しない: エージェントの標準入出力は、端末に直結する (goro run が os.Stdout などを、そのまま檻に渡す)。
// ログインの画面・エラー文・エスケープシーケンスは、そのまま (バイト単位で) 利用者に届き、goro の挙動 (終了コード・終了後の表示) は、
// その内容に左右されない。画面の内容を模倣・予告する案内は、版が変わると嘘になるので、書かない (TestUserMessagesStayShort)。
func TestAgentOutputPassesThroughUntouched(t *testing.T) {
	f := newRunFixture(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"claude (--repo)", []string{"run", "--repo", f.repo, "--", "screen"}},
		{"opencode (--repo)", []string{"run", "--agent", "opencode", "--repo", f.repo, "--", "screen"}},
		{"opencode (--login)", []string{"run", "--agent", "opencode", "--login", "--", "screen"}},
		{"claude (--login)", []string{"run", "--login", "--", "screen"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.goro(t, tc.args...)
			if r.stdout != screenStdout { // goro の標準出力には、エージェントの出力だけが、そのまま出る (goro 自身の表示は、標準エラーだけ)
				t.Errorf("標準出力が、エージェントの出力と、バイト単位で一致しない:\n got: %q\nwant: %q", r.stdout, screenStdout)
			}
			if !strings.Contains(r.stderr, screenStderr) {
				t.Errorf("標準エラーに、エージェントの出力がそのまま届いていない: %q\nwant を含む: %q", r.stderr, screenStderr)
			}
			if r.code != 3 { // 出力に "Failed"・"Timed out" があっても、goro が判断するのは、エージェントの終了コードだけ
				t.Errorf("終了コード = %d, want 3 (エージェントのもの)\n%s", r.code, r)
			}
		})
	}
}

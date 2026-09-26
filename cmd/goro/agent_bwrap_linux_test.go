package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 結合テスト (bwrap が要る): --agent opencode。偽の opencode は、偽の claude と同じテストバイナリ (argv[0] = opencode で再実行される。
// GORO_OPENCODE で指す)。檻の中の path・専用の HOME・環境変数・許可宛先が、opencode のものになり、claude の既定が変わらないことを確かめる。

// agentPath は、エージェント name の状態ディレクトリ (<state>/agents/<name>) の下の path (配置は、ここにリテラルで書いて固定する)。
func (f *runFixture) agentPath(name string, elem ...string) string {
	return filepath.Join(append([]string{f.stateDir(), "agents", name}, elem...)...)
}

// opencode の檻: /opt/opencode/opencode に実行ファイルが見え (/opt/claude/claude は無い)、専用の HOME (<state>/agents/opencode/home) が rw、
// ホストの opencode の認証情報・設定・状態ディレクトリは見えない。環境変数は、共通の 4 つ + OPENCODE_DISABLE_* だけ。
func TestRunOpenCodeCage(t *testing.T) {
	f := newRunFixture(t)
	hostAuth := filepath.Join(f.home, ".local", "share", "opencode", "auth.json")
	hostConf := filepath.Join(f.home, ".config", "opencode", "opencode.json")
	probes := []string{
		"stat:/opt/opencode/opencode", "stat:/opt/claude/claude", "stat:/opt/goro/goro", "dial:127.0.0.1:3128",
		"stat:" + hostAuth, "stat:" + filepath.Join(f.home, ".local", "share", "opencode"), "stat:" + hostConf,
		"stat:" + f.home, "stat:" + filepath.Join(f.home, ".ssh", "id_test"), "stat:" + f.stateDir(), "stat:" + f.repo,
		"write:/usr/x", "write:/run/goro/x",
		"write:/home/goro/x", "write:/work/x", "write:/tmp/x",
		"mnt:/opt/opencode/opencode", "mnt:/home/goro", "mnt:/work",
	}
	r := f.goro(t, append([]string{"run", "--agent", "opencode", "--repo", f.repo, "--", "probe"}, probes...)...).mustOK(t)
	_, res := parseOut(r.stdout)
	for op, want := range map[string]string{
		"stat:/opt/opencode/opencode": "ok", "stat:/opt/goro/goro": "ok", "dial:127.0.0.1:3128": "ok",
		"write:/home/goro/x": "ok", "write:/work/x": "ok", "write:/tmp/x": "ok",
		"mnt:/opt/opencode/opencode": "ro", "mnt:/home/goro": "rw", "mnt:/work": "rw",
	} {
		if res[op] != want {
			t.Errorf("%s = %q, want %q", op, res[op], want)
		}
	}
	for _, op := range []string{
		"stat:/opt/claude/claude", "stat:" + hostAuth, "stat:" + filepath.Join(f.home, ".local", "share", "opencode"), "stat:" + hostConf,
		"stat:" + f.home, "stat:" + filepath.Join(f.home, ".ssh", "id_test"), "stat:" + f.stateDir(), "stat:" + f.repo,
		"write:/usr/x", "write:/run/goro/x",
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
	info := f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--", "info").mustOK(t)
	kv, _ := parseOut(info.stdout)
	if kv["home"] != "/home/goro" || kv["cwd"] != "/work" || kv["https_proxy"] != "http://127.0.0.1:3128" || kv["args"] != `["info"]` {
		t.Errorf("HOME・cwd・HTTPS_PROXY・args = %q・%q・%q・%q", kv["home"], kv["cwd"], kv["https_proxy"], kv["args"])
	}
	allowed := map[string]bool{}
	for _, n := range []string{"HOME", "PATH", "TERM", "LANG", "OPENCODE_DISABLE_AUTOUPDATE", "OPENCODE_DISABLE_MODELS_FETCH",
		"OPENCODE_DISABLE_SHARE", "OPENCODE_DISABLE_LSP_DOWNLOAD", "HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "PWD"} {
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
	if kv["cwd"] != "/work" || kv["work"] != "" || kv["home"] != "/home/goro" {
		t.Errorf("cwd・/work の中身・HOME = %q・%q・%q (空の作業ディレクトリのはず)", kv["cwd"], kv["work"], kv["home"])
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
	for _, want := range []string{"opencode の auth login を起動する", "檻専用の HOME (ログイン状態が残る): " + f.agentPath("opencode", "home"), "goro run --agent opencode --repo PATH"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("--login の案内に %q が無い:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stderr, "Security notes") || strings.Contains(r.stderr, "セッション:") {
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
// --allow で足した宛先は、通る。拒否は、監査ログと、終了後の案内に出る (許可しなくてよいものは、説明つき)。
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
	for _, want := range []string{"許可の一覧に無く、拒否された宛先", "registry.npmjs.org:443 (1 回)", "models.opencode.ai:443 (1 回)", "許可しなくてよい",
		"github.com:443 (1 回)", "pacman -S ripgrep", "api.anthropic.com:443 (1 回)", "(例: --allow api.anthropic.com:443)"} {
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
	if strings.Contains(c.stderr, "許可しなくてよい") {
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
			!strings.Contains(run2.stderr, "goro export "+id+"\n") || !strings.Contains(run2.stderr, "/sessions で選ぶ") {
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
	refused(t, r, "このセッションは claude で作られた", "--agent opencode では使えない", "--repo から新しいセッションを作ってください")
	if _, err := os.Lstat(f.agentPath("opencode", "home")); err == nil {
		t.Error("断ったのに、opencode の HOME が作られた")
	}
	// --opencode を付けても、同じ (opencode では動かさない)。--agent を省略した --session に、動かさない側の実行ファイルを指すと断る。
	refused(t, f.goro(t, "run", "--session", cid, "--opencode", f.exe, "--", "info"), "--opencode は、--agent opencode のときだけ使える", "動かすエージェントは claude")

	// opencode が作ったセッション。
	oid := sessionID(t, f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--", "exit", "0").mustOK(t))
	if b, err := os.ReadFile(agentFile(oid)); err != nil || string(b) != "opencode\n" {
		t.Fatalf("opencode のセッションの記録 = %q, %v", b, err)
	}
	refused(t, f.goro(t, "run", "--agent", "claude", "--session", oid, "--", "info"), "このセッションは opencode で作られた", "--agent claude では使えない")
	refused(t, f.goro(t, "run", "--session", oid, "--claude", f.exe, "--", "info"), "--claude は、--agent claude のときだけ使える", "動かすエージェントは opencode")

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
	refused(t, f.goro(t, "run", "--agent", "opencode", "--session", cid, "--", "info"), "このセッションは claude で作られた")
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

// --login の作業ディレクトリ (/work) と run dir は、エージェントごとに別: 片方の --login の檻が /work に置いたものが、もう片方の
// --login の檻の /work に見えてはいけない (どちらの向きも)。run dir の egress.log (過去の拒否宛先) も、別。claude の名前は、そのまま。
func TestRunLoginDirsSeparateBothWays(t *testing.T) {
	f := newRunFixture(t)
	// opencode の --login の檻が、claude の設定を置く。
	f.goro(t, "run", "--agent", "opencode", "--login", "--", "commit", "settings.json", "PLANTED-BY-OPENCODE-LOGIN", "msg")
	if b, err := os.ReadFile(f.agentPath("opencode", "login-work", "settings.json")); err != nil || string(b) != "PLANTED-BY-OPENCODE-LOGIN" {
		t.Fatalf("前提: opencode の --login の檻が、/work (<state>/agents/opencode/login-work) に書けていない: %q, %v", b, err)
	}
	r := f.goro(t, "run", "--login").mustOK(t)
	if kv, _ := parseOut(r.stdout); kv["work"] != "" {
		t.Errorf("claude の --login の檻の /work に、opencode の --login の檻が置いたものが見える: work=%q\n%s", kv["work"], r)
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

// opencode がスクリプト (npm のラッパーなど) なら、檻を起こす前に断る (セッションも作らない)。--agent opencode に --claude は効かない。
func TestRunOpenCodeRejectsScriptAndWrongFlag(t *testing.T) {
	f := newRunFixture(t)
	script := filepath.Join(f.dir, "opencode-wrapper")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env node\nconsole.log('x')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--opencode", script)
	if r.code != 1 || !strings.Contains(r.stderr, "スクリプト") || !strings.Contains(r.stderr, "--opencode か GORO_OPENCODE") {
		t.Errorf("スクリプトの opencode:\n%s", r)
	}
	if ss := f.goro(t, "sessions").mustOK(t); !strings.Contains(ss.stdout, "セッションは無い") {
		t.Errorf("断ったのに、セッションが作られた:\n%s", ss.stdout)
	}
	r = f.goro(t, "run", "--agent", "opencode", "--repo", f.repo, "--claude", f.exe)
	if r.code != exitUsage || !strings.Contains(r.stderr, "--claude は、--agent claude のときだけ") {
		t.Errorf("--agent opencode の --claude:\n%s", r)
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

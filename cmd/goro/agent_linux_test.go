package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nananek/goronation/cmd/internal/session"
	"github.com/nananek/goronation/egress"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// 2 つの profile の値を、そのまま固定する。claude の値は、agentProfile を入れる前の固定の値と同じ (既定のエージェントの挙動が変わらない)。
func TestAgentProfiles(t *testing.T) {
	envNames := func(p agentProfile) string {
		var out []string
		for _, e := range p.env {
			out = append(out, e.Key+"="+e.Value)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		p        agentProfile
		name     string
		jailExe  string
		homeName string
		env      string
		hosts    []string
		exeEnv   string
		login    []string
	}{
		{claudeProfile, "claude", "/opt/claude/claude", "home",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1,DISABLE_TELEMETRY=1,DISABLE_ERROR_REPORTING=1,DISABLE_AUTOUPDATER=1,CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1",
			[]string{"api.anthropic.com:443", "platform.claude.com:443"}, "GORO_CLAUDE", nil},
		{opencodeProfile, "opencode", "/opt/opencode/opencode", "home-opencode",
			"OPENCODE_DISABLE_AUTOUPDATE=1,OPENCODE_DISABLE_MODELS_FETCH=1,OPENCODE_DISABLE_SHARE=1,OPENCODE_DISABLE_LSP_DOWNLOAD=1",
			[]string{"opencode.ai:443"}, "GORO_OPENCODE", []string{"auth", "login"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			if p.name != tc.name || p.jailExe != tc.jailExe || p.homeName != tc.homeName || p.exeEnv() != tc.exeEnv {
				t.Errorf("name・jailExe・homeName・exeEnv = %q・%q・%q・%q, want %q・%q・%q・%q",
					p.name, p.jailExe, p.homeName, p.exeEnv(), tc.name, tc.jailExe, tc.homeName, tc.exeEnv)
			}
			if got := envNames(p); got != tc.env {
				t.Errorf("env = %s\nwant %s", got, tc.env)
			}
			if got := p.hosts(); !slices.Equal(got, tc.hosts) {
				t.Errorf("hosts = %v, want %v", got, tc.hosts)
			}
			if !slices.Equal(p.loginArgs, tc.login) {
				t.Errorf("loginArgs = %v, want %v", p.loginArgs, tc.login)
			}
			if p.loginGuide == "" || p.exeExample == "" {
				t.Error("loginGuide・exeExample が空")
			}
			// 許可リストは、egress が受け付け、bwrap の資格情報らしい名前の検査に通る。
			if err := checkAllow(p.hosts()); err != nil {
				t.Errorf("hosts を egress が受け付けない: %v", err)
			}
			for _, e := range p.env {
				if _, err := (bwrap.Spec{Host: bwrap.Host{Home: "/home/u"}, Env: []bwrap.EnvVar{e}, Cmd: []string{"/bin/true"}}).Argv(); err != nil {
					t.Errorf("環境変数 %s を、bwrap が拒否する: %v", e.Key, err)
				}
			}
			// 説明を添える宛先は、既定の許可に入っていない (拒否されたときの説明だから)。
			for target, note := range p.denyNotes {
				if slices.Contains(p.hosts(), target) || note == "" {
					t.Errorf("denyNotes の %s: 既定の許可に入っている (%v)・説明が空 (%q)", target, slices.Contains(p.hosts(), target), note)
				}
			}
		})
	}
	// 2 つの profile は、混ざらない (別の檻専用の HOME・別の檻の中の path・別の許可)。
	a, b := claudeProfile, opencodeProfile
	if a.homeName == b.homeName || a.jailExe == b.jailExe || a.exeEnv() == b.exeEnv() || a.name == b.name {
		t.Errorf("profile が混ざる: %+v / %+v", a, b)
	}
	for _, h := range a.hosts() {
		if slices.Contains(b.hosts(), h) {
			t.Errorf("opencode の既定の許可に、claude の宛先 %s が入っている", h)
		}
	}
	for _, e := range b.env {
		if !strings.HasPrefix(e.Key, "OPENCODE_") {
			t.Errorf("opencode の環境変数 %s が、OPENCODE_ で始まらない (claude の変数が混ざった?)", e.Key)
		}
	}
	for _, e := range a.env {
		if strings.HasPrefix(e.Key, "OPENCODE_") {
			t.Errorf("claude の環境変数に、opencode の %s が混ざった", e.Key)
		}
	}
}

func TestAgentByName(t *testing.T) {
	for _, name := range []string{"claude", "opencode"} {
		if p, ok := agentByName(name); !ok || p.name != name {
			t.Errorf("agentByName(%q) = %q, %v", name, p.name, ok)
		}
	}
	for _, name := range []string{"", "Claude", "OpenCode", "claude ", "codex", "opencode2", "../claude"} {
		if p, ok := agentByName(name); ok {
			t.Errorf("agentByName(%q) = %q, want 未知", name, p.name)
		}
	}
	if agents[0].name != "claude" {
		t.Errorf("先頭 (既定) = %q, want claude", agents[0].name)
	}
	if got := agentNames(); got != "claude か opencode" {
		t.Errorf("agentNames = %q", got)
	}
	// 空の runOptions.agent は、claude (テストが、agent を書かずに作る runOptions でも、既定になる)。
	if p := (runOptions{}).profile(); p.name != "claude" {
		t.Errorf("runOptions{}.profile() = %q, want claude", p.name)
	}
	if p := (runOptions{agent: "opencode"}).profile(); p.name != "opencode" {
		t.Errorf("agent=opencode の profile = %q", p.name)
	}
}

func TestParseRunArgsAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want runOptions // agent・claude・opencode だけを比べる
		err  string
	}{
		{"既定は claude", []string{"--repo", "r"}, runOptions{agent: "claude"}, ""},
		{"--agent claude", []string{"--agent", "claude", "--repo", "r"}, runOptions{agent: "claude"}, ""},
		{"--agent opencode", []string{"--agent", "opencode", "--repo", "r"}, runOptions{agent: "opencode"}, ""},
		{"--agent opencode --login", []string{"--agent", "opencode", "--login", "--", "--extra"}, runOptions{agent: "opencode"}, ""},
		{"--agent opencode --session", []string{"--agent=opencode", "--session", "x"}, runOptions{agent: "opencode"}, ""},
		{"--opencode は opencode のとき", []string{"--agent", "opencode", "--opencode", "/o", "--repo", "r"}, runOptions{agent: "opencode", opencode: "/o"}, ""},
		{"--claude は claude のとき", []string{"--claude", "/c", "--repo", "r"}, runOptions{agent: "claude", claude: "/c"}, ""},
		{"未知のエージェント", []string{"--agent", "codex", "--repo", "r"}, runOptions{}, "claude か opencode"},
		{"空のエージェント", []string{"--agent", "", "--repo", "r"}, runOptions{}, "claude か opencode"},
		{"大文字", []string{"--agent", "OpenCode", "--repo", "r"}, runOptions{}, "claude か opencode"},
		{"opencode に --claude", []string{"--agent", "opencode", "--claude", "/c", "--repo", "r"}, runOptions{}, "--claude は、--agent claude のときだけ"},
		{"claude に --opencode", []string{"--opencode", "/o", "--repo", "r"}, runOptions{}, "--opencode は、--agent opencode のときだけ"},
		{"--login で省略は claude", []string{"--login"}, runOptions{agent: "claude"}, ""},
		{"--session で省略は、空 (セッションを作ったエージェントで動かす)", []string{"--session", "x"}, runOptions{agent: ""}, ""},
		{"--session と --agent claude", []string{"--session", "x", "--agent", "claude"}, runOptions{agent: "claude"}, ""},
		{"--session で省略: 動かす側が未定なので、--opencode は、ここでは断らない (doRun が断る)", []string{"--session", "x", "--opencode", "/o"}, runOptions{agent: "", opencode: "/o"}, ""},
		{"--session と --agent claude に --opencode", []string{"--session", "x", "--agent", "claude", "--opencode", "/o"}, runOptions{}, "--opencode は、--agent opencode のときだけ"},
		{"-- の後ろの --agent は、flag ではない", []string{"--repo", "r", "--", "--agent", "opencode"}, runOptions{agent: "claude"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got, err := parseRunArgs(tc.args, &stderr)
			if tc.err != "" {
				if err == nil {
					t.Fatalf("error にならない: %+v", got)
				}
				if !strings.Contains(stderr.String(), tc.err) {
					t.Errorf("stderr に %q が無い:\n%s", tc.err, stderr.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("error: %v\n%s", err, stderr.String())
			}
			if got.agent != tc.want.agent || got.claude != tc.want.claude || got.opencode != tc.want.opencode {
				t.Errorf("agent・claude・opencode = %q・%q・%q, want %q・%q・%q", got.agent, got.claude, got.opencode, tc.want.agent, tc.want.claude, tc.want.opencode)
			}
		})
	}
}

// opencode の実行ファイルの解決: PATH の opencode・GORO_OPENCODE・--opencode の順。スクリプトは、claude と同じく断る。
func TestResolveOpenCode(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "bin", "opencode.real")
	script := filepath.Join(dir, "bin", "opencode-wrapper")
	link := filepath.Join(dir, "bin", "opencode")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{real: "\x7fELF fake\n", script: "#!/usr/bin/env node\nrequire('./x')\n"} {
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("opencode.real", link); err != nil {
		t.Fatal(err)
	}
	pathLookup := func(name string) (string, error) {
		if name != "opencode" {
			t.Errorf("lookPath(%q), want opencode", name)
		}
		return link, nil
	}
	noLookup := func(string) (string, error) { return "", exec.ErrNotFound }

	for _, tc := range []struct {
		name          string
		flagVal, env  string
		look          func(string) (string, error)
		want, wantErr string
	}{
		{"PATH の opencode は、symlink を辿った実体", "", "", pathLookup, real, ""},
		{"環境変数が PATH に勝つ", "", real, pathLookup, real, ""},
		{"--opencode が環境変数に勝つ", link, script, pathLookup, real, ""},
		{"PATH に無い", "", "", noLookup, "", "opencode が見つからない (PATH に置くか、--opencode か GORO_OPENCODE で指す)"},
		{"存在しない", filepath.Join(dir, "none"), "", pathLookup, "", "opencode ("},
		{"スクリプト (npm のラッパーなど)", script, "", pathLookup, "", "スクリプト"},
		{"環境変数のスクリプト", "", script, pathLookup, "", "--opencode か GORO_OPENCODE"},
		{"スクリプトの例は、opencode の置き場", script, "", pathLookup, "", "~/.opencode/bin/opencode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAgentExe(opencodeProfile, tc.flagVal, tc.env, tc.look)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveAgentExe = %q, %v, want error に %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveAgentExe = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
	// claude のエラーの文言は、opencode の値に置き換わらない。
	_, err = resolveAgentExe(claudeProfile, "", "", noLookup)
	if err == nil || !strings.Contains(err.Error(), "claude が見つからない (PATH に置くか、--claude か GORO_CLAUDE で指す)") {
		t.Errorf("claude の見つからないエラー = %v", err)
	}
	_, err = resolveAgentExe(claudeProfile, script, "", pathLookup)
	if err == nil || !strings.Contains(err.Error(), "--claude か GORO_CLAUDE") || !strings.Contains(err.Error(), "/opt/claude-code/bin/claude") {
		t.Errorf("claude のスクリプトのエラー = %v", err)
	}
	var pe *os.PathError
	if _, err := resolveAgentExe(opencodeProfile, filepath.Join(dir, "none"), "", pathLookup); !errors.As(err, &pe) {
		t.Errorf("存在しない opencode の error が、原因 (PathError) を包んでいない: %v", err)
	}
}

// testOpenCodeCage は、ホストの HOME が /home/u の、opencode の檻の設定。
func testOpenCodeCage() cageConfig {
	c := testCage()
	c.Agent = opencodeProfile
	c.AgentExe = "/home/u/.opencode/bin/opencode"
	c.AgentHome = "/home/u/.local/state/goro/home-opencode"
	c.Args = []string{"--continue"}
	return c
}

// opencode の檻の argv を、丸ごと固定する。claude の golden (TestCageSpecGolden) との違いは、エージェントの実行ファイルの
// 行 (/opt/opencode/opencode)・専用の HOME (home-opencode)・環境変数 (OPENCODE_DISABLE_*)・起動するコマンドだけ。
func TestCageSpecGoldenOpenCode(t *testing.T) {
	argv, err := cageSpec(testOpenCodeCage()).Argv()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/bwrap", "--unshare-all", "--die-with-parent",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--proc", "/proc", "--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc/ssl/certs", "/etc/ssl/certs",
		"--ro-bind", "/home/u/.opencode/bin/opencode", "/opt/opencode/opencode",
		"--ro-bind", "/home/u/bin/goro", "/opt/goro/goro",
		"--ro-bind", "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/run", "/run/goro",
		"--bind", "/home/u/.local/state/goro/home-opencode", "/home/goro",
		"--bind", "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/clone", "/work",
		"--chdir", "/work",
		"--clearenv",
		"--setenv", "HOME", "/home/goro",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "TERM", "xterm-256color",
		"--setenv", "LANG", "C.UTF-8",
		"--setenv", "OPENCODE_DISABLE_AUTOUPDATE", "1",
		"--setenv", "OPENCODE_DISABLE_MODELS_FETCH", "1",
		"--setenv", "OPENCODE_DISABLE_SHARE", "1",
		"--setenv", "OPENCODE_DISABLE_LSP_DOWNLOAD", "1",
		"--",
		"/opt/goro/goro", "init", "--listen", "127.0.0.1:3128", "--upstream", "/run/goro/proxy.sock", "--no-forward-tty", "--",
		"/opt/opencode/opencode", "--continue",
	}
	if !slices.Equal(argv, want) {
		t.Errorf("argv が golden と違う:\n got: %q\nwant: %q", argv, want)
	}
	// claude の argv に、opencode のものが漏れない (同じ入力の檻で、エージェントだけが違う)。
	claude, err := cageSpec(testCage()).Argv()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range claude {
		if strings.Contains(a, "opencode") || strings.Contains(a, "OPENCODE") {
			t.Errorf("claude の argv に opencode が混ざっている: %q", a)
		}
	}
}

// opencode の檻の bind: rw は、専用の HOME と /work だけ。ホストの opencode の認証情報・設定・履歴の path は、bwrap が拒否する。
func TestCageSpecBindsOpenCode(t *testing.T) {
	type bind struct{ rw, inHome bool }
	got := map[string]bind{}
	for _, b := range cageSpec(testOpenCodeCage()).Binds {
		got[b.Dst] = bind{b.RW, b.InHome}
	}
	want := map[string]bind{
		"/usr": {false, false}, "/etc/ssl/certs": {false, false},
		"/opt/opencode/opencode": {false, true}, "/opt/goro/goro": {false, true}, "/run/goro": {false, true},
		"/home/goro": {true, true}, "/work": {true, true},
	}
	if len(got) != len(want) {
		t.Errorf("bind の数 = %d, want %d: %+v", len(got), len(want), got)
	}
	for dst, w := range want {
		if got[dst] != w {
			t.Errorf("%s: rw/InHome = %+v, want %+v", dst, got[dst], w)
		}
	}
	if _, ok := got["/opt/claude/claude"]; ok {
		t.Error("opencode の檻に、/opt/claude/claude がある")
	}
	for _, bad := range []struct{ what, exe, home string }{
		{"~/.local/share/opencode の下の実行ファイル", "/home/u/.local/share/opencode/bin/opencode", ""},
		{"~/.config/opencode の下の実行ファイル", "/home/u/.config/opencode/opencode", ""},
		{"~/.local/share/opencode を、エージェントの HOME に", "", "/home/u/.local/share/opencode"},
		{"~/.config/opencode を、エージェントの HOME に", "", "/home/u/.config/opencode"},
	} {
		c := testOpenCodeCage()
		if bad.exe != "" {
			c.AgentExe = bad.exe
		}
		if bad.home != "" {
			c.AgentHome = bad.home
		}
		if _, err := cageSpec(c).Argv(); err == nil {
			t.Errorf("%s を、bwrap が拒否しない", bad.what)
		}
	}
}

func TestCageArgsOpenCode(t *testing.T) {
	if got := cageArgs(runOptions{login: true}, opencodeProfile); !slices.Equal(got, []string{"auth", "login"}) {
		t.Errorf("opencode の --login = %v, want [auth login] (opencode に onboarding は無い。auth login は、自分で終わる)", got)
	}
	if got := cageArgs(runOptions{login: true, agentArgs: []string{"--extra"}}, opencodeProfile); !slices.Equal(got, []string{"auth", "login", "--extra"}) {
		t.Errorf("opencode の --login と引数 = %v", got)
	}
	if got := cageArgs(runOptions{repo: "r", agentArgs: []string{"--continue"}}, opencodeProfile); !slices.Equal(got, []string{"--continue"}) {
		t.Errorf("opencode の --repo と引数 = %v", got)
	}
	if got := cageArgs(runOptions{repo: "r"}, opencodeProfile); len(got) != 0 {
		t.Errorf("opencode の --repo = %v, want 引数なし", got)
	}
	// 呼ぶたびに、profile の loginArgs を書き換えない (append が、共有の配列に書かない)。
	for i := 0; i < 3; i++ {
		cageArgs(runOptions{login: true, agentArgs: []string{"x", "y", "z"}}, opencodeProfile)
	}
	if !slices.Equal(opencodeProfile.loginArgs, []string{"auth", "login"}) {
		t.Errorf("opencodeProfile.loginArgs が書き換わった: %v", opencodeProfile.loginArgs)
	}
	// claude の --login は、引数なし (素の対話起動) のまま。
	if got := cageArgs(runOptions{login: true}, claudeProfile); len(got) != 0 {
		t.Errorf("claude の --login = %v", got)
	}
}

func TestAllowListOpenCode(t *testing.T) {
	if got := allowList(opencodeProfile, nil); !slices.Equal(got, egress.OpenCodeHosts()) {
		t.Errorf("allowList(opencode, nil) = %v, want %v", got, egress.OpenCodeHosts())
	}
	got := allowList(opencodeProfile, []string{"api.anthropic.com:443", "opencode.ai:443", "api.anthropic.com:443"})
	if want := []string{"opencode.ai:443", "api.anthropic.com:443"}; !slices.Equal(got, want) {
		t.Errorf("allowList = %v, want %v (既定に、--allow を重複なく足す)", got, want)
	}
	for _, h := range allowList(opencodeProfile, nil) {
		if strings.Contains(h, "anthropic") || strings.Contains(h, "claude") {
			t.Errorf("opencode の既定の許可に、claude の宛先 %s が入っている", h)
		}
	}
	a := allowList(opencodeProfile, nil)
	a[0] = "tampered:1"
	if allowList(opencodeProfile, nil)[0] != "opencode.ai:443" || egress.OpenCodeHosts()[0] != "opencode.ai:443" {
		t.Error("allowList が、共有の slice を返している")
	}
}

// 終了後の案内: 既定 (claude) 以外は、再開・次のコマンドに --agent が付く。claude の案内は変わらない。
func TestPrintRunSummaryAgent(t *testing.T) {
	summary := func(s runSummary) string {
		var b bytes.Buffer
		s.started = true
		printRunSummary(&b, s)
		return b.String()
	}
	const id = "20260926-120000-abcdef"
	for _, tc := range []struct {
		name string
		s    runSummary
		want []string
		not  []string
	}{
		{"claude (agent を書かない)", runSummary{id: id},
			[]string{"  再開:         goro run --session " + id + "\n", "  成果の取り出し: goro export " + id + "\n"},
			[]string{"--agent", "opencode", "/sessions"}},
		{"claude (profile を書く)", runSummary{id: id, agent: claudeProfile},
			[]string{"  再開:         goro run --session " + id + "\n"}, []string{"--agent", "opencode"}},
		{"opencode", runSummary{id: id, agent: opencodeProfile},
			[]string{"  再開:         goro run --agent opencode --session " + id + "\n", "  成果の取り出し: goro export " + id + "\n", "/sessions で選ぶ"},
			[]string{"goro export --agent"}},
		{"opencode と --state-dir", runSummary{id: id, agent: opencodeProfile, stateDir: "/s t", stateDirGiven: true},
			[]string{"goro run --agent opencode --state-dir '/s t' --session " + id + "\n", "goro export --state-dir '/s t' " + id + "\n"}, nil},
		{"opencode の --login", runSummary{agent: opencodeProfile, agentHome: "/h"},
			[]string{"  次は: goro run --agent opencode --repo PATH\n", "檻専用の HOME (ログイン状態が残る): /h\n"}, []string{"セッション:", "/sessions"}},
		{"claude の --login", runSummary{agentHome: "/h"},
			[]string{"  次は: goro run --repo PATH\n"}, []string{"--agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := summary(tc.s)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("案内に %q が無い:\n%s", w, out)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("案内に %q がある:\n%s", n, out)
				}
			}
		})
	}
}

// 拒否された宛先のうち、説明のあるもの (opencode の registry.npmjs.org・github.com など) は、説明を添え、--allow の例に使わない。
// 一覧から隠しはしない (回数も出る)。claude は、これまで通り (説明なし。先頭の宛先が例)。
func TestPrintRunSummaryDenyNotes(t *testing.T) {
	summary := func(p agentProfile, denied []deniedTarget, more int) string {
		var b bytes.Buffer
		printRunSummary(&b, runSummary{id: "x", agent: p, started: true, denied: denied, deniedMore: more})
		return b.String()
	}
	npm := deniedTarget{"registry.npmjs.org:443", 2}
	models := deniedTarget{"models.opencode.ai:443", 1}
	evil := deniedTarget{"evil.example:443", 3}

	// 動作に影響しない宛先だけ: 一覧と説明は出るが、「必要な宛先」の案内は出ない。
	out := summary(opencodeProfile, []deniedTarget{npm, models}, 0)
	for _, want := range []string{"registry.npmjs.org:443 (2 回)", "models.opencode.ai:443 (1 回)", "許可しなくてよい", "@opencode-ai/plugin"} {
		if !strings.Contains(out, want) {
			t.Errorf("案内に %q が無い:\n%s", want, out)
		}
	}
	if strings.Contains(out, "必要な宛先は") || strings.Contains(out, "--allow registry") {
		t.Errorf("許可しなくてよい宛先だけなのに、--allow を案内している:\n%s", out)
	}
	// 本物の拒否が混ざる: 例は、その宛先 (先頭が npm でも)。その宛先には、説明を付けない。
	out = summary(opencodeProfile, []deniedTarget{npm, evil}, 0)
	if !strings.Contains(out, "(例: --allow evil.example:443)") {
		t.Errorf("--allow の例が、許可しなくてよい宛先になっている:\n%s", out)
	}
	if strings.Count(out, "許可しなくてよい") != 1 {
		t.Errorf("説明が、evil.example にも付いている (npm の 1 回だけのはず):\n%s", out)
	}
	// github.com (ripgrep の download): 許可を勧めず、rg を入れるよう案内する。例にも使わない。
	out = summary(opencodeProfile, []deniedTarget{{"github.com:443", 1}}, 0)
	for _, want := range []string{"github.com:443 (1 回)", "pacman -S ripgrep", "許可しないほうがよい"} {
		if !strings.Contains(out, want) {
			t.Errorf("ripgrep の案内に %q が無い:\n%s", want, out)
		}
	}
	if strings.Contains(out, "必要な宛先は") {
		t.Errorf("github.com だけなのに、--allow を案内している:\n%s", out)
	}
	// 一覧に出ない宛先が残っている (deniedMore) ときは、案内を出す (例は、宛先の形)。
	out = summary(opencodeProfile, []deniedTarget{npm}, 4)
	if !strings.Contains(out, "ほか 4 件") || !strings.Contains(out, "(例: --allow HOST:PORT)") {
		t.Errorf("ほか N 件があるのに、案内が無い:\n%s", out)
	}
	// claude: 説明 (denyNotes) が無い。同じ宛先でも、説明なしで、先頭の宛先が例 (変わらない)。
	out = summary(claudeProfile, []deniedTarget{npm, evil}, 0)
	if strings.Contains(out, "許可しなくてよい") || !strings.Contains(out, "(例: --allow registry.npmjs.org:443)") {
		t.Errorf("claude の案内が変わった:\n%s", out)
	}
	// 別の agent の説明は、効かない (opencode の説明が、claude の拒否に付かない)。
	if out := summary(claudeProfile, []deniedTarget{models}, 0); strings.Contains(out, "許可しなくてよい") {
		t.Errorf("claude の拒否に、opencode の説明が付いた:\n%s", out)
	}
}

// pickAgent: --session は、セッションを作ったエージェント (記録。無ければ claude) で動かす。--agent の省略は記録に従い、
// 記録と違う --agent は断る。--session でなければ、--agent (省略は claude) のまま。
func TestPickAgent(t *testing.T) {
	state := shortDir(t)
	store, err := session.NewStore(state, bwrap.Host{Home: "/home/u"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, record string, clone bool) {
		t.Helper()
		dir := filepath.Join(state, "sessions", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if clone {
			if err := os.Mkdir(filepath.Join(dir, "clone"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if record != "" {
			if err := os.WriteFile(filepath.Join(dir, "agent"), []byte(record), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("20260926-120000-aaaaaa", "", true) // 記録なし (エージェントを記録する前)
	mk("20260926-120001-bbbbbb", "claude\n", true)
	mk("20260926-120002-cccccc", "opencode\n", true)
	mk("20260926-120003-dddddd", "codex\n", true)     // この goro の知らないエージェント
	mk("20260926-120004-eeeeee", "Bad Agent\n", true) // 壊れた記録
	mk("20260926-120005-ffffff", "opencode\n", false) // clone が無い

	for _, tc := range []struct {
		name string
		o    runOptions
		want string // 空なら error
		err  string
	}{
		{"--session でない: 既定", runOptions{repo: "r"}, "claude", ""},
		{"--session でない: --agent", runOptions{repo: "r", agent: "opencode"}, "opencode", ""},
		{"--login", runOptions{login: true, agent: "opencode"}, "opencode", ""},
		{"記録なし・省略は claude", runOptions{session: "20260926-120000-aaaaaa"}, "claude", ""},
		{"記録なし・--agent claude", runOptions{session: "20260926-120000-aaaaaa", agent: "claude"}, "claude", ""},
		{"記録なし・--agent opencode は断る", runOptions{session: "20260926-120000-aaaaaa", agent: "opencode"}, "", "このセッションは claude で作られた"},
		{"claude・省略", runOptions{session: "20260926-120001-bbbbbb"}, "claude", ""},
		{"claude・--agent opencode は断る", runOptions{session: "20260926-120001-bbbbbb", agent: "opencode"}, "", "--agent opencode では使えない"},
		{"opencode・省略", runOptions{session: "20260926-120002-cccccc"}, "opencode", ""},
		{"opencode・--agent opencode", runOptions{session: "20260926-120002-cccccc", agent: "opencode"}, "opencode", ""},
		{"opencode・--agent claude は断る", runOptions{session: "20260926-120002-cccccc", agent: "claude"}, "", "このセッションは opencode で作られた"},
		{"知らないエージェントの記録", runOptions{session: "20260926-120003-dddddd"}, "", `"codex" を、この goro は知らない`},
		{"壊れた記録", runOptions{session: "20260926-120004-eeeeee"}, "", "エージェントの記録"},
		{"壊れた記録・--agent claude でも断る", runOptions{session: "20260926-120004-eeeeee", agent: "claude"}, "", "エージェントの記録"},
		{"clone が無い", runOptions{session: "20260926-120005-ffffff"}, "", "セッションを使えない"},
		{"存在しない", runOptions{session: "20260101-000000-aaaaaa"}, "", "セッションを使えない"},
		{"形が違う ID", runOptions{session: "../x"}, "", "セッションを使えない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sess, err := pickAgent(tc.o, store)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("pickAgent = %q, %v, want error に %q", p.name, err, tc.err)
				}
				if tc.o.session != "" && strings.Contains(tc.err, "で作られた") && !strings.Contains(err.Error(), "--repo から新しいセッションを作ってください") {
					t.Errorf("エラーに、次の手 (--repo から新しいセッション) が無い: %v", err)
				}
				return
			}
			if err != nil || p.name != tc.want {
				t.Fatalf("pickAgent = %q, %v, want %q", p.name, err, tc.want)
			}
			if (tc.o.session != "") != (sess != nil) || (sess != nil && sess.ID != tc.o.session) {
				t.Errorf("セッション = %+v (--session %q)", sess, tc.o.session)
			}
		})
	}
}

// --login の作業ディレクトリと run dir の名前は、エージェントごとに別 (claude は、これまでの名前のまま)。UDS の path も、その run dir の下。
func TestLoginDirs(t *testing.T) {
	work, run := claudeProfile.loginDirs()
	if work != "login-work" || run != "login-run" {
		t.Errorf("claude の loginDirs = %q・%q, want login-work・login-run (これまでの名前。互換)", work, run)
	}
	owork, orun := opencodeProfile.loginDirs()
	if owork != "login-work-opencode" || orun != "login-run-opencode" {
		t.Errorf("opencode の loginDirs = %q・%q", owork, orun)
	}
	if work == owork || run == orun {
		t.Error("login の作業ディレクトリか run dir が、エージェント間で共有されている")
	}
	for _, p := range agents { // どのエージェントも、名前が違い、セッションのディレクトリ (sessions) と衝突しない
		w, r := p.loginDirs()
		if w == r || w == "sessions" || r == "sessions" || w == p.homeName || r == p.homeName {
			t.Errorf("%s の loginDirs = %q・%q (互いに、HOME・sessions と衝突している)", p.name, w, r)
		}
	}
	if got := sockPathFor("/s", runOptions{login: true}); got != "/s/login-run/proxy.sock" {
		t.Errorf("claude の login の UDS = %q", got)
	}
	if got := sockPathFor("/s", runOptions{login: true, agent: "opencode"}); got != "/s/login-run-opencode/proxy.sock" {
		t.Errorf("opencode の login の UDS = %q", got)
	}
}

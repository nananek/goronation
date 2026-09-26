package bwrap

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
	"github.com/nananek/goronation/sandbox/contract"
)

// このファイルは、bwrap の Backend (契約のアダプタ) が、既存の API (Spec.Argv) と、同じ argv を作ることと、契約の共通検証 (sandbox/contract) が、
// bwrap の検証と食い違わないことを確認する。bwrap は要らない (Prepare は、何も起動しない)。

var adapterHost = contract.Host{Home: "/home/u"}

// agentSpec は、goro run の檻 (cmd/goro/cage.go の cageSpec) と同じ形の、契約の Spec。ホストの HOME は /home/u。
func agentSpec(name, exe, agentHome string, env []sandbox.EnvVar, args ...string) sandbox.Spec {
	const run = "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/run"
	return sandbox.Spec{
		Exec: "/opt/goro/goro",
		Args: append([]string{"init", "--listen", "127.0.0.1:3128", "--upstream", "/run/goro/proxy.sock", "--no-forward-tty", "--", "/opt/" + name + "/" + name}, args...),
		Dir:  "/work",
		Env: append([]sandbox.EnvVar{{Key: "HOME", Value: "/home/goro"}, {Key: "PATH", Value: "/usr/bin:/bin"},
			{Key: "TERM", Value: "xterm-256color"}, {Key: "LANG", Value: "C.UTF-8"}}, env...),
		System:  true,
		Scratch: []string{"/tmp"},
		Read: []sandbox.Mount{
			{HostPath: "/etc/ssl/certs", GuestPath: "/etc/ssl/certs"},
			{HostPath: exe, GuestPath: "/opt/" + name + "/" + name, InHome: true},
			{HostPath: "/home/u/bin/goro", GuestPath: "/opt/goro/goro", InHome: true},
			{HostPath: run, GuestPath: "/run/goro", InHome: true},
		},
		Write: []sandbox.Mount{
			{HostPath: agentHome, GuestPath: "/home/goro", InHome: true},
			{HostPath: "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/clone", GuestPath: "/work", InHome: true},
		},
		Egress:   "/run/goro/proxy.sock",
		Loopback: []string{"127.0.0.1:3128"},
		Terminal: true,
	}
}

// TestBackendPrepareAgentGolden は、goro run の檻 (claude・opencode) を契約の Spec で表して、Prepare に通した argv が、
// cmd/goro の golden (TestCageSpecGolden・TestCageSpecGoldenOpenCode) と、同一なことを確認する (アダプタは、argv を変えない)。
func TestBackendPrepareAgentGolden(t *testing.T) {
	claude := agentSpec("claude", "/home/u/.local/share/claude/versions/2.0.0", "/home/u/.local/state/goro/agents/claude/home", []sandbox.EnvVar{
		{Key: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Value: "1"}, {Key: "DISABLE_TELEMETRY", Value: "1"}, {Key: "DISABLE_ERROR_REPORTING", Value: "1"},
		{Key: "DISABLE_AUTOUPDATER", Value: "1"}, {Key: "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", Value: "1"},
	}, "--resume", "x y")
	opencode := agentSpec("opencode", "/home/u/.opencode/bin/opencode", "/home/u/.local/state/goro/agents/opencode/home", []sandbox.EnvVar{
		{Key: "OPENCODE_DISABLE_AUTOUPDATE", Value: "1"}, {Key: "OPENCODE_DISABLE_MODELS_FETCH", Value: "1"},
		{Key: "OPENCODE_DISABLE_SHARE", Value: "1"}, {Key: "OPENCODE_DISABLE_LSP_DOWNLOAD", Value: "1"},
	}, "--continue")
	claudeWant := []string{
		"/usr/bin/bwrap", "--unshare-all", "--die-with-parent",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--proc", "/proc", "--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc/ssl/certs", "/etc/ssl/certs",
		"--ro-bind", "/home/u/.local/share/claude/versions/2.0.0", "/opt/claude/claude",
		"--ro-bind", "/home/u/bin/goro", "/opt/goro/goro",
		"--ro-bind", "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/run", "/run/goro",
		"--bind", "/home/u/.local/state/goro/agents/claude/home", "/home/goro",
		"--bind", "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/clone", "/work",
		"--chdir", "/work",
		"--clearenv",
		"--setenv", "HOME", "/home/goro",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "TERM", "xterm-256color",
		"--setenv", "LANG", "C.UTF-8",
		"--setenv", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1",
		"--setenv", "DISABLE_TELEMETRY", "1",
		"--setenv", "DISABLE_ERROR_REPORTING", "1",
		"--setenv", "DISABLE_AUTOUPDATER", "1",
		"--setenv", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1",
		"--",
		"/opt/goro/goro", "init", "--listen", "127.0.0.1:3128", "--upstream", "/run/goro/proxy.sock", "--no-forward-tty", "--",
		"/opt/claude/claude", "--resume", "x y",
	}
	opencodeWant := []string{
		"/usr/bin/bwrap", "--unshare-all", "--die-with-parent",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--proc", "/proc", "--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc/ssl/certs", "/etc/ssl/certs",
		"--ro-bind", "/home/u/.opencode/bin/opencode", "/opt/opencode/opencode",
		"--ro-bind", "/home/u/bin/goro", "/opt/goro/goro",
		"--ro-bind", "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/run", "/run/goro",
		"--bind", "/home/u/.local/state/goro/agents/opencode/home", "/home/goro",
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
	for name, tc := range map[string]struct {
		spec sandbox.Spec
		want []string
	}{"claude": {claude, claudeWant}, "opencode": {opencode, opencodeWant}} {
		got, err := New(adapterHost).Prepare(tc.spec)
		if err != nil {
			t.Fatalf("%s: Prepare: %v", name, err)
		}
		if !slices.Equal(got.Argv, tc.want) {
			t.Errorf("%s の argv が golden と違う:\n got: %q\nwant: %q", name, got.Argv, tc.want)
		}
	}
}

// gitContract は、git の檻 (cmd/internal/session/cage.go の gitSpec) と同じ形の、契約の Spec。
func gitContract(read, write []sandbox.Mount, args ...string) sandbox.Spec {
	env := []sandbox.EnvVar{
		{Key: "HOME", Value: "/home/goro"}, {Key: "PATH", Value: "/usr/bin:/bin"}, {Key: "LANG", Value: "C.UTF-8"},
		{Key: "GIT_CONFIG_NOSYSTEM", Value: "1"}, {Key: "GIT_CONFIG_GLOBAL", Value: "/dev/null"}, {Key: "GIT_TERMINAL_PROMPT", Value: "0"},
		{Key: "GIT_CONFIG_COUNT", Value: "3"},
		{Key: "GIT_CONFIG_KEY_0", Value: "safe.directory"}, {Key: "GIT_CONFIG_VALUE_0", Value: "*"},
		{Key: "GIT_CONFIG_KEY_1", Value: "core.fsmonitor"}, {Key: "GIT_CONFIG_VALUE_1", Value: "false"},
		{Key: "GIT_CONFIG_KEY_2", Value: "core.hooksPath"}, {Key: "GIT_CONFIG_VALUE_2", Value: "/dev/null"},
	}
	return sandbox.Spec{Exec: "/usr/bin/git", Args: args, Dir: "/tmp", Env: env, System: true, Scratch: []string{"/tmp", "/home/goro"}, Read: read, Write: write}
}

// TestBackendPrepareGitGolden は、git の檻 (clone・remote remove・bundle) を契約の Spec で表して、Prepare に通した argv が、
// cmd/internal/session の gitSpec の argv (実物を、golden にしたもの) と同一なことを確認する。端末を持たない (Terminal = false) 檻は --new-session。
func TestBackendPrepareGitGolden(t *testing.T) {
	const sess = "/home/u/.local/state/goro/sessions/20260926-103000-a1b2c3"
	m := func(host, guest string) sandbox.Mount {
		return sandbox.Mount{HostPath: host, GuestPath: guest, InHome: true}
	}
	for name, tc := range map[string]struct {
		spec sandbox.Spec
		want []string
	}{
		"clone": {gitContract([]sandbox.Mount{m("/home/u/work/repo", "/src")}, []sandbox.Mount{m(sess+"/clone", "/work")},
			"clone", "--no-local", "--no-hardlinks", "-c", "user.name=goro", "-c", "user.email=goro@localhost.invalid", "--", "/src", "/work"), []string{
			"/usr/bin/bwrap",
			"--unshare-all",
			"--die-with-parent",
			"--new-session",
			"--symlink",
			"usr/lib",
			"/lib",
			"--symlink",
			"usr/lib64",
			"/lib64",
			"--symlink",
			"usr/bin",
			"/bin",
			"--symlink",
			"usr/sbin",
			"/sbin",
			"--proc",
			"/proc",
			"--dev",
			"/dev",
			"--tmpfs",
			"/tmp",
			"--tmpfs",
			"/home/goro",
			"--ro-bind",
			"/usr",
			"/usr",
			"--ro-bind",
			"/home/u/work/repo",
			"/src",
			"--bind",
			"/home/u/.local/state/goro/sessions/20260926-103000-a1b2c3/clone",
			"/work",
			"--chdir",
			"/tmp",
			"--clearenv",
			"--setenv",
			"HOME",
			"/home/goro",
			"--setenv",
			"PATH",
			"/usr/bin:/bin",
			"--setenv",
			"LANG",
			"C.UTF-8",
			"--setenv",
			"GIT_CONFIG_NOSYSTEM",
			"1",
			"--setenv",
			"GIT_CONFIG_GLOBAL",
			"/dev/null",
			"--setenv",
			"GIT_TERMINAL_PROMPT",
			"0",
			"--setenv",
			"GIT_CONFIG_COUNT",
			"3",
			"--setenv",
			"GIT_CONFIG_KEY_0",
			"safe.directory",
			"--setenv",
			"GIT_CONFIG_VALUE_0",
			"*",
			"--setenv",
			"GIT_CONFIG_KEY_1",
			"core.fsmonitor",
			"--setenv",
			"GIT_CONFIG_VALUE_1",
			"false",
			"--setenv",
			"GIT_CONFIG_KEY_2",
			"core.hooksPath",
			"--setenv",
			"GIT_CONFIG_VALUE_2",
			"/dev/null",
			"--",
			"/usr/bin/git",
			"clone",
			"--no-local",
			"--no-hardlinks",
			"-c",
			"user.name=goro",
			"-c",
			"user.email=goro@localhost.invalid",
			"--",
			"/src",
			"/work",
		}},
		"work": {gitContract(nil, []sandbox.Mount{m(sess+"/clone", "/work")}, "-C", "/work", "remote", "remove", "origin"), []string{
			"/usr/bin/bwrap",
			"--unshare-all",
			"--die-with-parent",
			"--new-session",
			"--symlink",
			"usr/lib",
			"/lib",
			"--symlink",
			"usr/lib64",
			"/lib64",
			"--symlink",
			"usr/bin",
			"/bin",
			"--symlink",
			"usr/sbin",
			"/sbin",
			"--proc",
			"/proc",
			"--dev",
			"/dev",
			"--tmpfs",
			"/tmp",
			"--tmpfs",
			"/home/goro",
			"--ro-bind",
			"/usr",
			"/usr",
			"--bind",
			"/home/u/.local/state/goro/sessions/20260926-103000-a1b2c3/clone",
			"/work",
			"--chdir",
			"/tmp",
			"--clearenv",
			"--setenv",
			"HOME",
			"/home/goro",
			"--setenv",
			"PATH",
			"/usr/bin:/bin",
			"--setenv",
			"LANG",
			"C.UTF-8",
			"--setenv",
			"GIT_CONFIG_NOSYSTEM",
			"1",
			"--setenv",
			"GIT_CONFIG_GLOBAL",
			"/dev/null",
			"--setenv",
			"GIT_TERMINAL_PROMPT",
			"0",
			"--setenv",
			"GIT_CONFIG_COUNT",
			"3",
			"--setenv",
			"GIT_CONFIG_KEY_0",
			"safe.directory",
			"--setenv",
			"GIT_CONFIG_VALUE_0",
			"*",
			"--setenv",
			"GIT_CONFIG_KEY_1",
			"core.fsmonitor",
			"--setenv",
			"GIT_CONFIG_VALUE_1",
			"false",
			"--setenv",
			"GIT_CONFIG_KEY_2",
			"core.hooksPath",
			"--setenv",
			"GIT_CONFIG_VALUE_2",
			"/dev/null",
			"--",
			"/usr/bin/git",
			"-C",
			"/work",
			"remote",
			"remove",
			"origin",
		}},
		"export": {gitContract([]sandbox.Mount{m(sess+"/clone", "/work")}, []sandbox.Mount{m(sess+"/export", "/out")},
			"-C", "/work", "bundle", "create", "/out/goro.bundle", "--all"), []string{
			"/usr/bin/bwrap",
			"--unshare-all",
			"--die-with-parent",
			"--new-session",
			"--symlink",
			"usr/lib",
			"/lib",
			"--symlink",
			"usr/lib64",
			"/lib64",
			"--symlink",
			"usr/bin",
			"/bin",
			"--symlink",
			"usr/sbin",
			"/sbin",
			"--proc",
			"/proc",
			"--dev",
			"/dev",
			"--tmpfs",
			"/tmp",
			"--tmpfs",
			"/home/goro",
			"--ro-bind",
			"/usr",
			"/usr",
			"--ro-bind",
			"/home/u/.local/state/goro/sessions/20260926-103000-a1b2c3/clone",
			"/work",
			"--bind",
			"/home/u/.local/state/goro/sessions/20260926-103000-a1b2c3/export",
			"/out",
			"--chdir",
			"/tmp",
			"--clearenv",
			"--setenv",
			"HOME",
			"/home/goro",
			"--setenv",
			"PATH",
			"/usr/bin:/bin",
			"--setenv",
			"LANG",
			"C.UTF-8",
			"--setenv",
			"GIT_CONFIG_NOSYSTEM",
			"1",
			"--setenv",
			"GIT_CONFIG_GLOBAL",
			"/dev/null",
			"--setenv",
			"GIT_TERMINAL_PROMPT",
			"0",
			"--setenv",
			"GIT_CONFIG_COUNT",
			"3",
			"--setenv",
			"GIT_CONFIG_KEY_0",
			"safe.directory",
			"--setenv",
			"GIT_CONFIG_VALUE_0",
			"*",
			"--setenv",
			"GIT_CONFIG_KEY_1",
			"core.fsmonitor",
			"--setenv",
			"GIT_CONFIG_VALUE_1",
			"false",
			"--setenv",
			"GIT_CONFIG_KEY_2",
			"core.hooksPath",
			"--setenv",
			"GIT_CONFIG_VALUE_2",
			"/dev/null",
			"--",
			"/usr/bin/git",
			"-C",
			"/work",
			"bundle",
			"create",
			"/out/goro.bundle",
			"--all",
		}},
	} {
		got, err := New(adapterHost).Prepare(tc.spec)
		if err != nil {
			t.Fatalf("%s: Prepare: %v", name, err)
		}
		if !slices.Equal(got.Argv, tc.want) {
			t.Errorf("git の檻 %s の argv が golden と違う:\n got: %q\nwant: %q", name, got.Argv, tc.want)
		}
	}
}

// bindDsts は、argv の --bind・--ro-bind の bind 先を、並んだ順に返す。
func bindDsts(argv []string) []string {
	var out []string
	for i, a := range argv {
		if (a == "--bind" || a == "--ro-bind") && i+2 < len(argv) {
			out = append(out, argv[i+2])
		}
	}
	return out
}

// TestBackendBindOrder は、mount の順序を確認する: 重ならない配置は、System・Read・Write の順のまま。重なるとき (Write の中の Read など) は、
// 外側が先 (内側を先に作ると、後から外側を bind したときに隠れて、ro のはずのものが書ける。実測 bwrap 0.12)。
func TestBackendBindOrder(t *testing.T) {
	b := New(adapterHost)
	spec := func(read, write []sandbox.Mount) sandbox.Spec {
		return sandbox.Spec{Exec: "/opt/x/x", System: true, Read: read, Write: write}
	}
	m := func(host, guest string) sandbox.Mount { return sandbox.Mount{HostPath: host, GuestPath: guest} }
	for name, tc := range map[string]struct {
		spec sandbox.Spec
		want []string
	}{
		"重ならない: System・Read・Write の順": {
			spec([]sandbox.Mount{m("/data/a", "/a"), m("/data/b", "/b")}, []sandbox.Mount{m("/data/w", "/w")}),
			[]string{"/usr", "/a", "/b", "/w"}},
		"Write の中の Read": {
			spec([]sandbox.Mount{m("/data/w/tools/agent", "/w/tools/agent")}, []sandbox.Mount{m("/data/w", "/w")}),
			[]string{"/usr", "/w", "/w/tools/agent"}},
		"Write の中の Read が 2 つ": {
			spec([]sandbox.Mount{m("/data/w/a", "/w/a"), m("/data/w/b", "/w/b")}, []sandbox.Mount{m("/data/w", "/w")}),
			[]string{"/usr", "/w", "/w/a", "/w/b"}},
		"Read の中の Read": {
			spec([]sandbox.Mount{m("/data/x/y", "/x/y"), m("/data/x", "/x")}, nil),
			[]string{"/usr", "/x", "/x/y"}},
		"/usr の下の Read": {
			spec([]sandbox.Mount{m("/usr/local/bin/goro", "/usr/local/bin/goro")}, nil),
			[]string{"/usr", "/usr/local/bin/goro"}},
		"名前が似ているだけ (/w2 は /w の下でない)": {
			spec([]sandbox.Mount{m("/data/w2", "/w2")}, []sandbox.Mount{m("/data/w", "/w")}),
			[]string{"/usr", "/w2", "/w"}},
	} {
		got, err := b.Prepare(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if dsts := bindDsts(got.Argv); !slices.Equal(dsts, tc.want) {
			t.Errorf("%s: bind 先の順 = %q, want %q", name, dsts, tc.want)
		}
	}
}

// TestBackendPrepareRejects は、契約の共通検証と、bwrap の検証の、どちらに反しても、Prepare が ErrRejected を包んだ error を返すことを確認する。
func TestBackendPrepareRejects(t *testing.T) {
	b := New(adapterHost)
	base := func() sandbox.Spec { return sandbox.Spec{Exec: "/opt/x/x", System: true} }
	m := func(host string) sandbox.Mount { return sandbox.Mount{HostPath: host, GuestPath: "/x"} }
	for name, mutate := range map[string]func(*sandbox.Spec){
		"HOME 自体 (契約)": func(s *sandbox.Spec) { s.Read = []sandbox.Mount{m("/home/u")} },
		"~/.ssh (契約)": func(s *sandbox.Spec) {
			s.Read = []sandbox.Mount{{HostPath: "/home/u/.ssh", GuestPath: "/x", InHome: true}}
		},
		"資格情報らしい環境変数 (契約)":                func(s *sandbox.Spec) { s.Env = []sandbox.EnvVar{{Key: "GH_TOKEN", Value: "x"}} },
		"GuestPath が /proc の中 (bwrap 固有)": func(s *sandbox.Spec) { s.Read = []sandbox.Mount{{HostPath: "/data/x", GuestPath: "/proc/x"}} },
		"GuestPath が /dev の中 (bwrap 固有)":  func(s *sandbox.Spec) { s.Read = []sandbox.Mount{{HostPath: "/data/x", GuestPath: "/dev/x"}} },
		"Scratch が /proc の中 (bwrap 固有)":   func(s *sandbox.Spec) { s.Scratch = []string{"/proc/x"} },
		"Egress の親が Read に無い (契約のみ)":      func(s *sandbox.Spec) { s.Egress = "/run/goro/proxy.sock" },
		"loopback でない待ち受け (契約のみ)":         func(s *sandbox.Spec) { s.Loopback = []string{"0.0.0.0:3128"} },
		"Read と Write の GuestPath の重複":    func(s *sandbox.Spec) { s.Read = []sandbox.Mount{m("/data/a")}; s.Write = []sandbox.Mount{m("/data/b")} },
	} {
		s := base()
		mutate(&s)
		_, err := b.Prepare(s)
		if !errors.Is(err, sandbox.ErrRejected) {
			t.Errorf("%s: Prepare = %v, want ErrRejected", name, err)
		}
	}
	if _, err := b.Prepare(base()); err != nil {
		t.Errorf("最小の Spec を断った: %v", err)
	}
}

// TestBackendDeclaration は、bwrap の宣言と名前を確認する。
func TestBackendDeclaration(t *testing.T) {
	b := New(adapterHost)
	c := b.Capabilities()
	if b.Name() != "bwrap" || !c.PathRemap || !c.PrivateLoopback || !c.KillsDescendants || !c.PrivatePIDs || !slices.Equal(c.ExtraEnv, []string{"PWD"}) {
		t.Errorf("Name = %q, Capabilities = %+v", b.Name(), c)
	}
}

// TestContractTablesMatchBwrap は、契約の共通検証 (sandbox/contract) の、機密の表が、bwrap の表と同じことを確認する
// (契約の側が弱いと、契約を使う別のバックエンドで、機密が通る。表を、片方だけ変えられない)。
func TestContractTablesMatchBwrap(t *testing.T) {
	p := contract.LinuxPolicy()
	for name, pair := range map[string][2][]string{
		"ProtectedTrees": {p.ProtectedTrees, protectedTrees}, "WholeDenied": {p.WholeDenied, wholeDenied},
		"ReadOnlyTrees": {p.ReadOnlyTrees, readOnlyTrees}, "HomeSecrets": {contract.HomeSecrets(), homeSecrets},
	} {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s が違う:\n contract: %q\n bwrap:    %q", name, pair[0], pair[1])
		}
	}
	keys, prefixes, parts := contract.CredEnv()
	if !slices.Equal(keys, credEnvKeys) || !slices.Equal(prefixes, credEnvPrefixes) || !slices.Equal(parts, credEnvParts) {
		t.Errorf("資格情報らしい環境変数の規則が違う:\n contract: %q %q %q\n bwrap:    %q %q %q", keys, prefixes, parts, credEnvKeys, credEnvPrefixes, credEnvParts)
	}
}

// TestContractAgreesWithBwrap は、同じ入力に対して、契約の共通検証と、bwrap の検証が、同じ判定 (通す・断る) をすることを、表で確認する。
// 契約が断らないものを bwrap が断るのは、bwrap の追加の検証。逆 (契約が通して、bwrap が断る) が無いことが、この表で確かめる範囲。
func TestContractAgreesWithBwrap(t *testing.T) {
	host := contract.Host{Home: "/home/tester", Secrets: []string{"/run/user/1000/agent.sock", "/tmp/agent.sock"}}
	rules := contract.Rules{Host: host, Policy: contract.LinuxPolicy(), Caps: New(host).Capabilities()}
	bhost := Host{Home: host.Home, Secrets: host.Secrets}
	paths := []string{
		"/", "/home", "/home/tester", "/home/other", "/home/tester/work/x", "/home/tester/.ssh", "/home/tester/.ssh/id", "/home/tester/.sshx",
		"/home/tester/.claude", "/home/tester/.claude.json", "/home/tester/.claude-code", "/home/tester/.gnupg", "/home/tester/.gnupg-vault",
		"/home/tester/.config", "/home/tester/.config/gh", "/home/tester/.config/opencode", "/home/tester/.local", "/home/tester/.local/share",
		"/home/tester/.local/share/opencode", "/home/tester/.opencode/bin/opencode", "/home/tester/.aws", "/home/tester/.netrc", "/home/tester/.cache",
		"/root", "/etc", "/etc/ssl/certs", "/etc/shadow", "/etc/ssh", "/etc/ssh/x", "/etc/passwd", "/run", "/run/user/1000", "/run/user/1000/agent.sock",
		"/run/goro", "/var", "/var/run", "/var/lib/docker", "/var/tmp", "/var/tmp/x", "/proc", "/proc/self", "/sys", "/dev", "/dev/shm", "/boot",
		"/tmp", "/tmp/x", "/tmp/agent.sock", "/usr", "/usr/bin/git", "/usr/local/bin/goro", "/bin", "/lib", "/opt", "/opt/claude/claude", "/data", "/data/x", "/srv/x",
		"rel", "", "/a/../b", "/a\nb",
	}
	n := 0
	for _, p := range paths {
		for _, inHome := range []bool{false, true} {
			for _, write := range []bool{false, true} {
				bs := Spec{Host: bhost, Binds: []Bind{{Src: p, Dst: "/x", RW: write, InHome: inHome}}, Cmd: []string{"/bin/true"}}
				_, berr := bs.Argv()
				cs := sandbox.Spec{Exec: "/bin/true"}
				mm := sandbox.Mount{HostPath: p, GuestPath: "/x", InHome: inHome}
				if write {
					cs.Write = []sandbox.Mount{mm}
				} else {
					cs.Read = []sandbox.Mount{mm}
				}
				cerr := rules.Validate(cs)
				if (berr == nil) != (cerr == nil) {
					t.Errorf("%q (write=%v inHome=%v): bwrap = %v, contract = %v", p, write, inHome, berr, cerr)
				}
				n++
			}
		}
	}
	envs := []sandbox.EnvVar{
		{Key: "HOME", Value: "/x"}, {Key: "PATH", Value: "/usr/bin"}, {Key: "TERM", Value: "xterm"}, {Key: "LANG", Value: "C"}, {Key: "_X", Value: ""}, {Key: "a1", Value: "v"},
		{Key: "SSH_AUTH_SOCK", Value: "/x"}, {Key: "ssh_agent_pid", Value: "1"}, {Key: "GNUPGHOME", Value: "/x"}, {Key: "GPG_AGENT_INFO", Value: "x"}, {Key: "KUBECONFIG", Value: "x"},
		{Key: "AWS_REGION", Value: "x"}, {Key: "aws_profile", Value: "x"}, {Key: "AZURE_X", Value: "x"}, {Key: "GOOGLE_X", Value: "x"}, {Key: "GCP_X", Value: "x"}, {Key: "CLOUDSDK_X", Value: "x"},
		{Key: "GH_TOKEN", Value: "x"}, {Key: "github_token", Value: "x"}, {Key: "ANTHROPIC_API_KEY", Value: "x"}, {Key: "OPENAI_APIKEY", Value: "x"}, {Key: "X_SECRET", Value: "x"},
		{Key: "DB_PASSWORD", Value: "x"}, {Key: "DB_PASSWD", Value: "x"}, {Key: "CREDENTIALS", Value: "x"}, {Key: "PRIVATE_KEY", Value: "x"}, {Key: "ACCESS_KEY_ID", Value: "x"},
		{Key: "", Value: "x"}, {Key: "1A", Value: "x"}, {Key: "A B", Value: "x"}, {Key: "A=B", Value: "x"}, {Key: "A-B", Value: "x"}, {Key: "OPENCODE_DISABLE_SHARE", Value: "1"},
		{Key: "A", Value: "x\x00y"}, {Key: "A", Value: "x\ny"}, {Key: "A", Value: "x\ry"}, {Key: "A", Value: "ok value"},
	}
	for _, e := range envs {
		bs := Spec{Host: bhost, Env: []EnvVar{{Key: e.Key, Value: e.Value}}, Cmd: []string{"/bin/true"}}
		_, berr := bs.Argv()
		cerr := rules.Validate(sandbox.Spec{Exec: "/bin/true", Env: []sandbox.EnvVar{e}})
		if (berr == nil) != (cerr == nil) {
			t.Errorf("環境変数 %q=%q: bwrap = %v, contract = %v", e.Key, e.Value, berr, cerr)
		}
		n++
	}
	for _, exec := range []string{"/bin/true", "true", "", "/a/../b", "/a\nb", "/a b"} {
		_, berr := Spec{Host: bhost, Cmd: []string{exec}}.Argv()
		cerr := rules.Validate(sandbox.Spec{Exec: exec})
		if (berr == nil) != (cerr == nil) {
			t.Errorf("Exec %q: bwrap = %v, contract = %v", exec, berr, cerr)
		}
		n++
	}
	if n < 250 {
		t.Errorf("比べた数 = %d, want 250 以上 (表が空振りしていないか)", n)
	}
}

// TestBackendUsesLinuxPolicy は、アダプタが、契約の検証に、Linux の表 (LinuxPolicy) と、bwrap の宣言 (Capabilities) と、渡された Host を使うことを確認する
// (配線を誤ると、契約の検証が、機密の表を持たないまま通す。bwrap 自身の検証は、別に働くので、他のテストでは見えない)。
func TestBackendUsesLinuxPolicy(t *testing.T) {
	b := New(adapterHost)
	want := contract.LinuxPolicy()
	if !slices.Equal(b.rules.Policy.ProtectedTrees, want.ProtectedTrees) || !slices.Equal(b.rules.Policy.ReadOnlyTrees, want.ReadOnlyTrees) ||
		!slices.Equal(b.rules.Policy.WholeDenied, want.WholeDenied) || !slices.Equal(b.rules.Policy.GuestReserved, want.GuestReserved) ||
		!slices.Equal(b.rules.Policy.SystemPaths, want.SystemPaths) || !slices.Equal(b.rules.Policy.SystemLinks, want.SystemLinks) {
		t.Errorf("契約の検証の Policy = %+v, want LinuxPolicy", b.rules.Policy)
	}
	if b.rules.Host.Home != adapterHost.Home || b.rules.Caps.PathRemap != b.Capabilities().PathRemap {
		t.Errorf("契約の検証の Host・Caps = %+v・%+v", b.rules.Host, b.rules.Caps)
	}
	// 契約の検証が、この表で、bwrap の検証より先に断る (文言で、どちらが断ったかを見る)。
	_, err := b.Prepare(sandbox.Spec{Exec: "/opt/x/x", Read: []sandbox.Mount{{HostPath: "/etc/ssh", GuestPath: "/x"}}})
	if !errors.Is(err, sandbox.ErrRejected) || !strings.Contains(err.Error(), "HostPath") {
		t.Errorf("/etc/ssh: %v, want 契約の検証 (HostPath) の ErrRejected", err)
	}
}

// TestSystemPlacementMatchesPolicy は、アダプタが System で作る配置 (/usr の bind と 4 つの symlink) が、契約の表 (Policy の SystemPaths・SystemLinks) と、
// 同じことを確認する (表が違うと、契約の検証は、基盤が占める path を、取り違える)。/proc・/dev を常に作ることも、表 (GuestReserved) と同じ。
func TestSystemPlacementMatchesPolicy(t *testing.T) {
	b := New(adapterHost)
	bs := b.translate(sandbox.Spec{Exec: "/opt/x/x", System: true})
	var links, binds []string
	for _, l := range bs.Symlinks {
		links = append(links, l.Dst)
	}
	for _, bd := range bs.Binds {
		binds = append(binds, bd.Dst)
	}
	pol := contract.LinuxPolicy()
	if !slices.Equal(links, pol.SystemLinks) || !slices.Equal(binds, pol.SystemPaths) {
		t.Errorf("System の配置: symlink %q・bind %q, want Policy の %q・%q", links, binds, pol.SystemLinks, pol.SystemPaths)
	}
	argv, err := b.translate(sandbox.Spec{Exec: "/opt/x/x"}).Argv()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pol.GuestReserved {
		if !slices.Contains(argv, p) {
			t.Errorf("bwrap が常に作る path %s が、argv に無い (表 GuestReserved と食い違う)", p)
		}
	}
}

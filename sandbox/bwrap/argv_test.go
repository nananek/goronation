package bwrap

import (
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// testHost は、検証に使うホストの状態 (実環境ではなく、固定の値)。
var testHost = Host{
	Home:    "/home/tester",
	Secrets: []string{"/tmp/ssh-XXXX/agent.1", "/run/user/1000/gnupg"},
}

const (
	testHome  = "/home/tester"
	testState = testHome + "/.local/state/goro/s1" // 作業用 dir (clone・檻専用 HOME・run dir) を置く場所
)

// cageSpec は、goro run の檻の Spec (spike の cage-final.sh と同じ形)。
func cageSpec() Spec {
	return Spec{
		Host: testHost,
		Symlinks: []Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		Tmpfs: []string{"/tmp"},
		Binds: []Bind{
			{Src: "/usr", Dst: "/usr"},
			{Src: "/etc/ssl/certs", Dst: "/etc/ssl/certs"},
			{Src: testHome + "/.local/share/claude/versions/2.1.283", Dst: "/opt/claude/claude", InHome: true},
			{Src: "/usr/local/bin/goro", Dst: "/opt/goro/goro"},
			{Src: testState + "/run", Dst: "/run/goro", InHome: true},
			{Src: testState + "/home", Dst: "/home/goro", RW: true, InHome: true},
			{Src: testState + "/clone", Dst: "/work", RW: true, InHome: true},
		},
		Env: []EnvVar{
			{"HOME", "/home/goro"}, {"PATH", "/usr/bin:/bin"}, {"TERM", "xterm-256color"}, {"LANG", "C.UTF-8"},
			{"HTTPS_PROXY", "http://127.0.0.1:3128"}, {"HTTP_PROXY", "http://127.0.0.1:3128"},
			{"NO_PROXY", "127.0.0.1,localhost"}, {"SSL_CERT_FILE", "/etc/ssl/certs/ca-certificates.crt"},
			{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"}, {"DISABLE_TELEMETRY", "1"},
		},
		Chdir: "/work",
		Cmd: []string{"/opt/goro/goro", "init", "--listen", "127.0.0.1:3128", "--upstream", "/run/goro/proxy.sock",
			"--", "/opt/claude/claude"},
	}
}

// cageArgv は、cageSpec が作る argv。別の実装で書いた期待値 (golden) で、bwrap に渡る文字列の全体を固定する。
var cageArgv = []string{
	"/usr/bin/bwrap", "--unshare-all", "--die-with-parent",
	"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
	"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
	"--proc", "/proc", "--dev", "/dev",
	"--tmpfs", "/tmp",
	"--ro-bind", "/usr", "/usr",
	"--ro-bind", "/etc/ssl/certs", "/etc/ssl/certs",
	"--ro-bind", "/home/tester/.local/share/claude/versions/2.1.283", "/opt/claude/claude",
	"--ro-bind", "/usr/local/bin/goro", "/opt/goro/goro",
	"--ro-bind", "/home/tester/.local/state/goro/s1/run", "/run/goro",
	"--bind", "/home/tester/.local/state/goro/s1/home", "/home/goro",
	"--bind", "/home/tester/.local/state/goro/s1/clone", "/work",
	"--chdir", "/work",
	"--clearenv",
	"--setenv", "HOME", "/home/goro", "--setenv", "PATH", "/usr/bin:/bin",
	"--setenv", "TERM", "xterm-256color", "--setenv", "LANG", "C.UTF-8",
	"--setenv", "HTTPS_PROXY", "http://127.0.0.1:3128", "--setenv", "HTTP_PROXY", "http://127.0.0.1:3128",
	"--setenv", "NO_PROXY", "127.0.0.1,localhost", "--setenv", "SSL_CERT_FILE", "/etc/ssl/certs/ca-certificates.crt",
	"--setenv", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1", "--setenv", "DISABLE_TELEMETRY", "1",
	"--",
	"/opt/goro/goro", "init", "--listen", "127.0.0.1:3128", "--upstream", "/run/goro/proxy.sock", "--", "/opt/claude/claude",
}

func TestArgvGolden(t *testing.T) {
	got, err := cageSpec().Argv()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cageArgv) {
		t.Errorf("argv が golden と違う:\n got %q\nwant %q", got, cageArgv)
	}
}

func TestArgvNewSession(t *testing.T) {
	s := cageSpec()
	if a, _ := s.Argv(); slices.Contains(a, "--new-session") {
		t.Errorf("既定で --new-session が付いている: %q", a)
	}
	s.NewSession = true
	a, err := s.Argv()
	if err != nil {
		t.Fatal(err)
	}
	// --new-session は、固定の 3 つの後ろ (mount より前) に 1 個だけ付く。
	want := append([]string{"/usr/bin/bwrap", "--unshare-all", "--die-with-parent", "--new-session"}, cageArgv[3:]...)
	if !reflect.DeepEqual(a, want) {
		t.Errorf("NewSession の argv:\n got %q\nwant %q", a, want)
	}
}

// flagArity は、Argv が出す bwrap のオプションと、その引数の数。これに無いオプションは、出してはいけない。
var flagArity = map[string]int{
	"--unshare-all": 0, "--die-with-parent": 0, "--new-session": 0, "--clearenv": 0,
	"--symlink": 2, "--proc": 1, "--dev": 1, "--tmpfs": 1, "--ro-bind": 2, "--bind": 2, "--chdir": 1, "--setenv": 2,
}

// parsedArgv は、argv を、Spec の各欄に読み戻したもの。
type parsedArgv struct {
	flags               []string // 引数の無いオプション (出た順)
	symlinks, binds     [][]string
	tmpfs, dev, proc    []string
	env                 [][]string
	chdir               []string
	cmd                 []string
	orderClearenvBefore bool // --clearenv が、すべての --setenv より前
}

// parseArgv は、argv を、bwrap と同じ規則 (オプションごとの引数の数) で読む。表に無いオプションは error にする。
func parseArgv(t *testing.T, argv []string) parsedArgv {
	t.Helper()
	var p parsedArgv
	if argv[0] != "/usr/bin/bwrap" {
		t.Fatalf("argv[0] = %q, want /usr/bin/bwrap (固定パス)", argv[0])
	}
	seenClear, ok := false, true
	i := 1
	for ; i < len(argv) && argv[i] != "--"; i++ {
		f := argv[i]
		n, known := flagArity[f]
		if !known || i+n >= len(argv) {
			t.Fatalf("表に無い・引数の足りないオプション %q (位置 %d): %q", f, i, argv)
		}
		args := slices.Clone(argv[i+1 : i+1+n])
		switch f {
		case "--symlink":
			p.symlinks = append(p.symlinks, args)
		case "--ro-bind", "--bind":
			p.binds = append(p.binds, append([]string{f}, args...))
		case "--tmpfs":
			p.tmpfs = append(p.tmpfs, args...)
		case "--dev":
			p.dev = append(p.dev, args...)
		case "--proc":
			p.proc = append(p.proc, args...)
		case "--setenv":
			ok = ok && seenClear
			p.env = append(p.env, args)
		case "--chdir":
			p.chdir = append(p.chdir, args...)
		case "--clearenv":
			seenClear = true
			p.flags = append(p.flags, f)
		default:
			p.flags = append(p.flags, f)
		}
		i += n
	}
	if i >= len(argv) {
		t.Fatalf("-- が無い: %q", argv)
	}
	p.orderClearenvBefore = ok
	p.cmd = argv[i+1:]
	return p
}

// TestArgvStructure は、通った Spec の argv が、常に、固定のオプションと、Spec の欄だけから成ることを確認する。
// 値が -- で始まる・空白を含むなど、オプションに見える文字列を入れても、bwrap のオプションにならない。
func TestArgvStructure(t *testing.T) {
	odd := cageSpec()
	odd.Env = append(odd.Env,
		EnvVar{"FLAGLIKE", "--share-net"}, EnvVar{"SPACE", "a b  c"}, EnvVar{"EQ", "a=b=c"}, EnvVar{"EMPTY", ""})
	odd.Symlinks = append(odd.Symlinks, Symlink{Target: "--dev-bind", Dst: "/weird"})
	odd.Cmd = append(odd.Cmd, "--unshare-all", "--share-net", "--ro-bind", "/", "/")
	odd.NewSession = true
	for name, s := range map[string]Spec{"cage": cageSpec(), "odd": odd} {
		t.Run(name, func(t *testing.T) {
			argv, err := s.Argv()
			if err != nil {
				t.Fatal(err)
			}
			p := parseArgv(t, argv)
			wantFlags := []string{"--unshare-all", "--die-with-parent", "--clearenv"}
			if s.NewSession {
				wantFlags = []string{"--unshare-all", "--die-with-parent", "--new-session", "--clearenv"}
			}
			if !reflect.DeepEqual(p.flags, wantFlags) {
				t.Errorf("引数の無いオプション = %q, want %q (--unshare-all・--die-with-parent・--clearenv は常に 1 個ずつ)", p.flags, wantFlags)
			}
			if !reflect.DeepEqual(p.proc, []string{"/proc"}) || !reflect.DeepEqual(p.dev, []string{"/dev"}) {
				t.Errorf("--proc %q --dev %q, want /proc と /dev を 1 個ずつ", p.proc, p.dev)
			}
			if !p.orderClearenvBefore {
				t.Errorf("--clearenv が、--setenv より後にある: %q", argv)
			}
			if len(p.symlinks) != len(s.Symlinks) || len(p.binds) != len(s.Binds) || len(p.env) != len(s.Env) ||
				len(p.tmpfs) != len(s.Tmpfs) {
				t.Errorf("Spec の欄と、argv の件数が違う: %+v", p)
			}
			for i, b := range s.Binds {
				flag := "--ro-bind"
				if b.RW {
					flag = "--bind"
				}
				if !reflect.DeepEqual(p.binds[i], []string{flag, b.Src, b.Dst}) {
					t.Errorf("Binds[%d] = %q, want %v", i, p.binds[i], b)
				}
			}
			for i, e := range s.Env {
				if !reflect.DeepEqual(p.env[i], []string{e.Key, e.Value}) {
					t.Errorf("Env[%d] = %q, want %v", i, p.env[i], e)
				}
			}
			if !reflect.DeepEqual(p.cmd, s.Cmd) {
				t.Errorf("-- の後ろ = %q, want Cmd %q", p.cmd, s.Cmd)
			}
			// ホストの / を丸ごと見せる形は、どの位置にも無い。
			for _, b := range p.binds {
				if b[1] == "/" || b[2] == "/" {
					t.Errorf("/ を bind している: %q", b)
				}
			}
		})
	}
}

// specCase は、拒否される Spec の 1 例。want は、エラー文に含まれるべき語 (別の規則が代わりに拒否した場合を見分ける)。
type specCase struct {
	name string
	mut  func(*Spec)
	want string
}

// withBind は、cageSpec に bind を 1 つ足す。
func withBind(b Bind) func(*Spec) {
	b.Dst = "/extra"
	return func(s *Spec) { s.Binds = append(s.Binds, b) }
}

func withEnv(k, v string) func(*Spec) {
	return func(s *Spec) { s.Env = append(s.Env, EnvVar{k, v}) }
}

func TestArgvRejectsSecretSrc(t *testing.T) {
	var cases []specCase
	// ホストの HOME と、その中の機密 (InHome を付けても error)。
	for _, src := range []string{
		testHome,
		testHome + "/.ssh", testHome + "/.ssh/id_ed25519",
		testHome + "/.claude", testHome + "/.claude/.credentials.json", testHome + "/.claude.json", testHome + "/.claude.json.backup",
		testHome + "/.gnupg", testHome + "/.gnupg-vault",
		testHome + "/.aws", testHome + "/.azure", testHome + "/.kube", testHome + "/.docker",
		testHome + "/.netrc", testHome + "/.git-credentials", testHome + "/.npmrc", testHome + "/.pypirc", testHome + "/.password-store",
		testHome + "/.codex", testHome + "/.copilot", testHome + "/.commandcode",
		testHome + "/.config/gh", testHome + "/.config/gh/hosts.yml", testHome + "/.config/git", testHome + "/.config/gcloud",
		testHome + "/.config/op", testHome + "/.local/share/keyrings",
		// 機密を含む親 (HOME 自体は上で見る)。
		testHome + "/.config", testHome + "/.local/share",
	} {
		want := "機密"
		if src == testHome+"/.config" || src == testHome+"/.local/share" {
			want = "HOME の下の機密"
		}
		cases = append(cases, specCase{"InHome でも " + src, withBind(Bind{Src: src, InHome: true}), want})
		cases = append(cases, specCase{"InHome なしで " + src, withBind(Bind{Src: src}), "機密"})
	}
	// HOME の親・ホスト共有の領域・システムの機密。
	for _, src := range []string{
		"/", "/root", "/root/.ssh", "/home", "/home/other", "/home/other/.ssh", "/home/tester2/x",
		"/etc", "/etc/shadow", "/etc/gshadow", "/etc/ssh", "/etc/ssh/ssh_host_ed25519_key", "/etc/sudoers", "/etc/sudoers.d",
		"/etc/ssl", "/etc/ssl/private", "/etc/ssl/private/x.key",
		"/run", "/run/user", "/run/user/1000", "/run/user/1000/bus", "/run/user/1000/goro", "/run/docker.sock", "/run/containerd",
		"/var", "/var/run", "/var/run/docker.sock", "/var/lib", "/var/lib/docker", "/var/lib/containerd/x",
		"/proc", "/proc/1/root", "/sys", "/sys/kernel", "/dev", "/dev/sda", "/dev/mem", "/boot", "/boot/grub",
	} {
		cases = append(cases, specCase{"読み取り専用でも " + src, withBind(Bind{Src: src}), "機密"})
		cases = append(cases, specCase{"書き込みで " + src, withBind(Bind{Src: src, RW: true}), "機密"})
	}
	// ホスト全体で共有する dir (丸ごとは不可。中は可)。
	for _, src := range []string{"/tmp", "/var/tmp"} {
		cases = append(cases, specCase{"丸ごと " + src, withBind(Bind{Src: src}), "共有する dir"})
	}
	// SSH_AUTH_SOCK など、実環境から得た認証用 socket・鍵 (Host.Secrets)。その親と配下も。
	for _, src := range []string{"/tmp/ssh-XXXX/agent.1", "/tmp/ssh-XXXX", "/tmp/ssh-XXXX/agent.1/x", "/run/user/1000/gnupg"} {
		cases = append(cases, specCase{"Host.Secrets " + src, withBind(Bind{Src: src}), "機密"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { expectRejected(t, c) })
	}
}

// TestArgvRejectsHomeAndParents は、HOME が /home の外にあっても、HOME 自体と、それを含む親を bind できないことを確認する
// (/home や / は、別の規則でも拒否されるので、HOME が /home の下だと、この規則を確かめられない)。
func TestArgvRejectsHomeAndParents(t *testing.T) {
	host := Host{Home: "/data/users/tester"}
	for _, src := range []string{"/data/users/tester", "/data/users", "/data"} {
		for _, inHome := range []bool{false, true} {
			s := cageSpec()
			s.Host = host
			s.Binds = []Bind{{Src: src, Dst: "/x", InHome: inHome}}
			if _, err := s.Argv(); err == nil || !strings.Contains(err.Error(), "機密") {
				t.Errorf("Src %q (InHome %v): error = %v, want 機密の path", src, inHome, err)
			}
		}
	}
	// HOME の下の作業用 dir は通る。HOME の兄弟は、この規則の対象ではない (別のユーザーの HOME を守るのは、/home の規則)。
	s := cageSpec()
	s.Host = host
	s.Binds = []Bind{{Src: "/data/users/tester/work", Dst: "/x", InHome: true}, {Src: "/data/other", Dst: "/y"}}
	if _, err := s.Argv(); err != nil {
		t.Errorf("正当な Spec が error: %v", err)
	}
}

func TestArgvRejectsBindMisuse(t *testing.T) {
	cases := []specCase{
		// HOME の下は、InHome を明示したときだけ。HOME の外に InHome は付けられない。
		{"HOME の下に InHome なし", withBind(Bind{Src: testHome + "/work"}), "InHome を明示"},
		{"HOME の外に InHome", withBind(Bind{Src: "/usr", InHome: true}), "HOME"},
		// システムの領域は ro だけ。
		{"/usr を rw", withBind(Bind{Src: "/usr", RW: true}), "ro でだけ"},
		{"/usr/local を rw", withBind(Bind{Src: "/usr/local", RW: true}), "ro でだけ"},
		{"/opt/x を rw", withBind(Bind{Src: "/opt/x", RW: true}), "ro でだけ"},
		{"/etc/ssl/certs を rw", withBind(Bind{Src: "/etc/ssl/certs", RW: true}), "ro でだけ"},
		{"/var/cache を rw", withBind(Bind{Src: "/var/cache", RW: true}), "ro でだけ"},
		{"/bin を rw", withBind(Bind{Src: "/bin", RW: true}), "ro でだけ"},
		{"/sbin を rw", withBind(Bind{Src: "/sbin", RW: true}), "ro でだけ"},
		{"/lib を rw", withBind(Bind{Src: "/lib", RW: true}), "ro でだけ"},
		{"/lib32 を rw", withBind(Bind{Src: "/lib32", RW: true}), "ro でだけ"},
		{"/lib64 を rw", withBind(Bind{Src: "/lib64", RW: true}), "ro でだけ"},
		{"/libx32 を rw", withBind(Bind{Src: "/libx32", RW: true}), "ro でだけ"},
		// path の形。
		{"Src が空", withBind(Bind{}), "path が空"},
		{"Src が相対", withBind(Bind{Src: "usr"}), "絶対ではない"},
		{"Src が ..", withBind(Bind{Src: "/usr/../etc"}), "クリーンではない"},
		{"Src が末尾 /", withBind(Bind{Src: "/usr/"}), "クリーンではない"},
		{"Src に改行", withBind(Bind{Src: "/usr/a\nb"}), "制御文字"},
		{"Src に NUL", withBind(Bind{Src: "/usr/a\x00b"}), "制御文字"},
		{"Src に DEL", withBind(Bind{Src: "/usr/a\x7fb"}), "制御文字"},
		{"Src にタブ", withBind(Bind{Src: "/usr/a\tb"}), "制御文字"},
		{"Dst が相対", func(s *Spec) { s.Binds[0].Dst = "usr" }, "絶対ではない"},
		{"Dst が ..", func(s *Spec) { s.Binds[0].Dst = "/a/../usr" }, "クリーンではない"},
		{"Dst が /", func(s *Spec) { s.Binds[0].Dst = "/" }, "使えない"},
		{"Dst が /proc の中", func(s *Spec) { s.Binds[0].Dst = "/proc/x" }, "使えない"},
		{"Dst が /dev", func(s *Spec) { s.Binds[0].Dst = "/dev" }, "使えない"},
		{"Dst が /dev の中", func(s *Spec) { s.Binds[0].Dst = "/dev/shm" }, "使えない"},
		{"Dst が重複 (bind と bind)", func(s *Spec) { s.Binds[1].Dst = s.Binds[0].Dst }, "重複"},
		{"Dst が重複 (bind と symlink)", func(s *Spec) { s.Symlinks[0].Dst = s.Binds[0].Dst }, "重複"},
		{"Dst が重複 (bind と tmpfs)", func(s *Spec) { s.Tmpfs = append(s.Tmpfs, s.Binds[0].Dst) }, "重複"},
		{"Dst が重複 (tmpfs と tmpfs)", func(s *Spec) { s.Tmpfs = append(s.Tmpfs, s.Tmpfs[0]) }, "重複"},
		{"symlink の Target が空", func(s *Spec) { s.Symlinks[0].Target = "" }, "Target"},
		{"symlink の Target に改行", func(s *Spec) { s.Symlinks[0].Target = "a\nb" }, "Target"},
		{"symlink の Dst が相対", func(s *Spec) { s.Symlinks[0].Dst = "lib" }, "絶対ではない"},
		{"tmpfs が /proc の中", func(s *Spec) { s.Tmpfs = append(s.Tmpfs, "/proc/x") }, "使えない"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { expectRejected(t, c) })
	}
}

func TestArgvRejectsEnv(t *testing.T) {
	var cases []specCase
	// 資格情報らしい名前は、呼び出し側が渡しても出せない (大文字小文字を問わない)。
	for _, k := range []string{
		"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GPG_AGENT_INFO", "GNUPGHOME", "KUBECONFIG",
		"GH_TOKEN", "GITHUB_TOKEN", "GITLAB_TOKEN", "NPM_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE", "AWS_REGION",
		"AZURE_TENANT_ID", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GCP_PROJECT", "CLOUDSDK_CORE_PROJECT",
		"github_token", "ssh_auth_sock", "Aws_Profile",
		"MY_TOKEN", "MY_SECRET", "DB_PASSWORD", "DB_PASSWD", "MY_CREDENTIAL", "MY_CREDENTIALS_FILE", "SERVICE_API_KEY", "SERVICE_APIKEY",
		"MY_PRIVATE_KEY", "MY_ACCESS_KEY",
	} {
		cases = append(cases, specCase{"資格情報らしい " + k, withEnv(k, "x"), "資格情報らしい"})
	}
	for _, c := range []struct{ name, k, v string }{
		{"名前が空", "", "x"},
		{"名前に =", "A=B", "x"},
		{"名前に NUL", "A\x00B", "x"},
		{"名前に改行", "A\nB", "x"},
		{"名前に空白", "A B", "x"},
		{"名前が数字で始まる", "1A", "x"},
		{"名前に - ", "A-B", "x"},
		{"名前が非 ASCII", "Ａ", "x"},
		{"値に NUL", "A", "x\x00y"},
		{"値に改行", "A", "x\ny"},
		{"値に CR", "A", "x\ry"},
	} {
		want := "英数字と _"
		if strings.HasPrefix(c.name, "値") {
			want = "NUL か改行"
		}
		cases = append(cases, specCase{c.name, withEnv(c.k, c.v), want})
	}
	cases = append(cases, specCase{"名前の重複", func(s *Spec) { s.Env = append(s.Env, s.Env[0]) }, "重複"})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { expectRejected(t, c) })
	}
}

func TestArgvRejectsCmdAndHost(t *testing.T) {
	cases := []specCase{
		{"Cmd が空", func(s *Spec) { s.Cmd = nil }, "Cmd が空"},
		{"Cmd[0] が相対 (PATH を検索しない)", func(s *Spec) { s.Cmd[0] = "claude" }, "絶対ではない"},
		{"Cmd[0] がクリーンでない", func(s *Spec) { s.Cmd[0] = "/opt/../opt/x" }, "クリーンではない"},
		{"Cmd の引数に NUL", func(s *Spec) { s.Cmd = append(s.Cmd, "a\x00b") }, "NUL"},
		{"Chdir が相対", func(s *Spec) { s.Chdir = "work" }, "絶対ではない"},
		{"Chdir がクリーンでない", func(s *Spec) { s.Chdir = "/work/" }, "クリーンではない"},
		{"Host.Home が空 (検証できない)", func(s *Spec) { s.Host = Host{} }, "Host"},
		{"Host.Home が相対", func(s *Spec) { s.Host.Home = "home" }, "Host"},
		{"Host.Home が /", func(s *Spec) { s.Host.Home = "/" }, "Host"},
		{"Host.Home がクリーンでない", func(s *Spec) { s.Host.Home = "/home/tester/" }, "Host"},
		{"Host.Secrets が相対", func(s *Spec) { s.Host.Secrets = []string{"agent"} }, "Host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { expectRejected(t, c) })
	}
}

func expectRejected(t *testing.T, c specCase) {
	t.Helper()
	s := cageSpec()
	c.mut(&s)
	argv, err := s.Argv()
	if err == nil {
		t.Fatalf("error にならない。argv = %q", argv)
	}
	if argv != nil {
		t.Errorf("error なのに argv を返した: %q", argv)
	}
	if !strings.Contains(err.Error(), c.want) {
		t.Errorf("error = %q。%q を含むはず (別の規則が代わりに拒否した)", err, c.want)
	}
}

// TestArgvAccepts は、正当な Spec を、拒否しすぎないことを確認する (拒否リストが広すぎて、使えなくなる変異を見つける)。
func TestArgvAccepts(t *testing.T) {
	binds := []Bind{
		{Src: "/usr"}, {Src: "/etc/ssl/certs"}, {Src: "/usr/local/bin/goro"}, {Src: "/opt/tool"},
		{Src: "/tmp/work", RW: true}, {Src: "/tmp/x/y/z", RW: true}, {Src: "/var/tmp/work"}, {Src: "/data/work", RW: true},
		{Src: "/srv/clone", RW: true}, {Src: "/mnt/clone", RW: true},
		{Src: testHome + "/.local/share/claude/versions/2.1.283", InHome: true},
		{Src: testHome + "/work", RW: true, InHome: true}, {Src: testHome + "/.local/state/goro", RW: true, InHome: true},
		{Src: testHome + "/.config/goro", InHome: true}, {Src: testHome + "/.cache/goro/x", RW: true, InHome: true},
		// 名前が、拒否する path の接頭辞に似ているだけの path は、拒否しない (path の要素の単位で比べる)。
		{Src: "/runtime/x"}, {Src: "/homework/x"}, {Src: "/rooted/x"}, {Src: "/etc/sshd_config"}, {Src: "/tmp2/x"}, {Src: "/varx/x"},
		{Src: "/procfs/x"}, {Src: "/sysroot/x"}, {Src: "/devices/x"}, {Src: "/bootstrap/x"},
		{Src: "/opt2/x", RW: true}, {Src: "/usr2/x", RW: true}, {Src: "/etcetera/x", RW: true}, {Src: "/binaries/x", RW: true},
		{Src: "/home/tester/.sshfoo-not", InHome: true}, // 先頭の要素が、機密の名前に似ているだけ (.ssh ではない)
		{Src: testHome + "/.config/gitea", InHome: true},
	}
	for _, b := range binds {
		s := cageSpec()
		b.Dst = "/extra"
		s.Binds = append(s.Binds, b)
		if _, err := s.Argv(); err != nil {
			t.Errorf("%+v が error: %v", b, err)
		}
	}
	// 正当な環境変数 (資格情報らしい名前を含まない。値は何でもよい)。
	for _, e := range []EnvVar{
		{"PATH", "/usr/bin:/bin"}, {"HTTPS_PROXY", "http://127.0.0.1:3128"}, {"SSL_CERT_FILE", "/etc/ssl/certs/ca.crt"},
		{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"}, {"DISABLE_TELEMETRY", "1"}, {"DISABLE_ERROR_REPORTING", "1"},
		{"DISABLE_AUTOUPDATER", "1"}, {"TERM", "xterm-256color"}, {"LANG", "C.UTF-8"}, {"_x9", "a=b c"}, {"a", ""},
		{"NO_PROXY", "127.0.0.1,localhost"}, {"CLAUDE_CONFIG_DIR", "/home/goro/.claude"},
	} {
		s := cageSpec()
		s.Env = []EnvVar{e}
		if _, err := s.Argv(); err != nil {
			t.Errorf("%+v が error: %v", e, err)
		}
	}
	// 検証に通る、最小の Spec。
	if _, err := (Spec{Host: testHost, Cmd: []string{"/bin/true"}}).Argv(); err != nil {
		t.Errorf("最小の Spec が error: %v", err)
	}
}

// TestCurrentHost は、CurrentHost が、実環境の値から、Host を作ることを確認する。
func TestCurrentHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-AAAA/agent.1")
	t.Setenv("GNUPGHOME", "/home/other/.gnupg")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("GPG_AGENT_INFO", "/tmp/gpg-BBBB/S.gpg-agent:1234:1")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus,guid=abc")
	h := CurrentHost()
	if h.Home != home {
		t.Errorf("Home = %q, want %q (HOME 環境変数)", h.Home, home)
	}
	for _, want := range []string{"/tmp/ssh-AAAA/agent.1", "/home/other/.gnupg", "/run/user/1000", "/tmp/gpg-BBBB/S.gpg-agent", "/run/user/1000/bus"} {
		if !slices.Contains(h.Secrets, want) {
			t.Errorf("Secrets = %q に、%q が無い", h.Secrets, want)
		}
	}
	if u, err := user.Current(); err == nil && filepath.Clean(u.HomeDir) != home && !slices.Contains(h.Secrets, filepath.Clean(u.HomeDir)) {
		t.Errorf("passwd の HOME %q が、Secrets %q に無い (HOME 環境変数と違う)", u.HomeDir, h.Secrets)
	}
	if err := h.check(); err != nil {
		t.Errorf("CurrentHost の値を、検証が拒否した: %v", err)
	}

	// HOME 環境変数が、passwd の HOME と同じなら、Secrets に入れない (入れると、HOME の下の作業用 dir を、すべて拒否してしまう)。
	if u, err := user.Current(); err == nil && filepath.IsAbs(u.HomeDir) {
		t.Setenv("HOME", u.HomeDir)
		if h := CurrentHost(); slices.Contains(h.Secrets, h.Home) {
			t.Errorf("Home %q が Secrets %q に入っている", h.Home, h.Secrets)
		}
	}

	// 相対 path・空の値は、Secrets に入れない。HOME が無ければ、Home が空になる (Argv が error にする)。
	t.Setenv("SSH_AUTH_SOCK", "agent.sock")
	t.Setenv("GNUPGHOME", "")
	t.Setenv("GPG_AGENT_INFO", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:abstract=/tmp/dbus-x")
	t.Setenv("XDG_RUNTIME_DIR", "")
	for _, s := range CurrentHost().Secrets {
		if s == "agent.sock" || s == "" || s == "." {
			t.Errorf("Secrets に、絶対 path でない値 %q がある", s)
		}
	}
}

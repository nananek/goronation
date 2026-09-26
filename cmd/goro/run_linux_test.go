package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/session"
	"github.com/nananek/goronation/egress"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// shortDir は、UDS を置ける短い path の、空のディレクトリを返す (UDS の path は 107 バイトまで)。
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "goro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestParseRunArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want runOptions
		err  string // 空でなければ、error になり、stderr にこれを含む
	}{
		{"--repo", []string{"--repo", "/r"}, runOptions{repo: "/r"}, ""},
		{"--session", []string{"--session", "20260926-120000-abcdef"}, runOptions{session: "20260926-120000-abcdef"}, ""},
		{"--login", []string{"--login"}, runOptions{login: true}, ""},
		{"全部", []string{"--repo", "r", "--name", "N", "--email", "e@x.invalid", "--state-dir", "/s", "--claude", "/c",
			"--allow", "a.example:443", "--allow", "b.example:8443", "--", "--resume", "x"},
			runOptions{repo: "r", name: "N", email: "e@x.invalid", stateDir: "/s", claude: "/c",
				allow: []string{"a.example:443", "b.example:8443"}, claudeArgs: []string{"--resume", "x"}}, ""},
		{"-- の後ろは、flag として読まない", []string{"--login", "--", "--repo", "--allow", "x"},
			runOptions{login: true, claudeArgs: []string{"--repo", "--allow", "x"}}, ""},
		{"-- の後ろが空", []string{"--repo", "r", "--"}, runOptions{repo: "r", claudeArgs: []string{}}, ""},
		{"モードが無い", []string{"--allow", "a.example:443"}, runOptions{}, "ちょうど 1 つ"},
		{"モードが 2 つ (repo と login)", []string{"--repo", "r", "--login"}, runOptions{}, "ちょうど 1 つ"},
		{"モードが 2 つ (repo と session)", []string{"--repo", "r", "--session", "x"}, runOptions{}, "ちょうど 1 つ"},
		{"モードが 3 つ", []string{"--repo", "r", "--session", "x", "--login"}, runOptions{}, "ちょうど 1 つ"},
		{"--name は --repo のときだけ", []string{"--login", "--name", "n"}, runOptions{}, "--repo のときだけ"},
		{"--email は --repo のときだけ", []string{"--session", "x", "--email", "e@x.invalid"}, runOptions{}, "--repo のときだけ"},
		{"-- の無い余計な引数", []string{"--repo", "r", "extra"}, runOptions{}, `余計な引数 "extra"`},
		{"--allow に port が無い", []string{"--login", "--allow", "host.example"}, runOptions{}, "host:port"},
		{"--allow のワイルドカード", []string{"--login", "--allow", "*.example.com:443"}, runOptions{}, "host:port"},
		{"--allow の IP リテラル", []string{"--login", "--allow", "1.2.3.4:443"}, runOptions{}, "host:port"},
		{"--allow の userinfo", []string{"--login", "--allow", "u@host.example:443"}, runOptions{}, "host:port"},
		{"--allow の port が 0", []string{"--login", "--allow", "host.example:0"}, runOptions{}, "host:port"},
		{"未知のフラグ", []string{"--login", "--bogus"}, runOptions{}, "bogus"},
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
			if got.repo != tc.want.repo || got.session != tc.want.session || got.login != tc.want.login || got.name != tc.want.name ||
				got.email != tc.want.email || got.stateDir != tc.want.stateDir || got.claude != tc.want.claude ||
				!slices.Equal(got.allow, tc.want.allow) || !slices.Equal(got.claudeArgs, tc.want.claudeArgs) {
				t.Errorf("parseRunArgs = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseRunArgsHelp(t *testing.T) {
	var stderr bytes.Buffer
	if _, err := parseRunArgs([]string{"-h"}, &stderr); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h の error = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(stderr.String(), "使い方: goro run") || !strings.Contains(stderr.String(), "--allow") {
		t.Errorf("使い方が出ていない:\n%s", stderr.String())
	}
	if code := runRun([]string{"-h"}, io.Discard); code != 0 {
		t.Errorf("runRun -h の終了コード = %d", code)
	}
	if code := runRun([]string{}, io.Discard); code != exitUsage {
		t.Errorf("runRun (引数なし) の終了コード = %d, want %d", code, exitUsage)
	}
}

// 許可の一覧は、既定 (claude の 2 宛先) に、--allow を重複なく足したもので、既定を外せない。checkAllow は、egress の検証そのもの。
func TestAllowList(t *testing.T) {
	if got := allowList(nil); !slices.Equal(got, egress.ClaudeHosts()) {
		t.Errorf("allowList(nil) = %v, want %v", got, egress.ClaudeHosts())
	}
	got := allowList([]string{"x.example:443", "api.anthropic.com:443", "x.example:443", "y.example:8443"})
	want := []string{"api.anthropic.com:443", "platform.claude.com:443", "x.example:443", "y.example:8443"}
	if !slices.Equal(got, want) {
		t.Errorf("allowList = %v, want %v", got, want)
	}
	a := allowList(nil)
	a[0] = "tampered:1"
	if egress.ClaudeHosts()[0] != "api.anthropic.com:443" || allowList(nil)[0] != "api.anthropic.com:443" {
		t.Error("allowList が、共有の slice を返している")
	}
	for _, ok := range []string{"a.example:443", "A.Example:443", "a-b.example.co:8443"} {
		if err := checkAllow([]string{ok}); err != nil {
			t.Errorf("checkAllow(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "a.example", "a.example:", "a.example:0", "a.example:65536", "a.example:x", "1.2.3.4:443", "[::1]:443",
		"*.example.com:443", "u@a.example:443", "a b.example:443", ":443"} {
		if err := checkAllow([]string{bad}); !errors.Is(err, egress.ErrConfig) {
			t.Errorf("checkAllow(%q) = %v, want ErrConfig", bad, err)
		}
	}
}

// export・sessions の使い方 (-h) と、引数の誤り。状態ディレクトリは、テストごとに別 (存在しない ID は、檻を起動する前に断る)。
func TestExportSessionsUsageAndArgs(t *testing.T) {
	state := shortDir(t)
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string // stdout か stderr に含まれる
	}{
		{"export -h", []string{"export", "-h"}, 0, "使い方: goro export"},
		{"sessions -h", []string{"sessions", "-h"}, 0, "使い方: goro sessions"},
		{"run -h", []string{"run", "-h"}, 0, "使い方: goro run"},
		{"export の ID が無い", []string{"export", "--state-dir", state}, exitUsage, "セッション ID を 1 つ"},
		{"export の ID が 2 つ", []string{"export", "--state-dir", state, "a", "b"}, exitUsage, "セッション ID を 1 つ"},
		{"export の未知のフラグ", []string{"export", "--bogus"}, exitUsage, "bogus"},
		{"存在しない ID", []string{"export", "--state-dir", state, "20260101-000000-aaaaaa"}, 1, "goro export:"},
		{"形が違う ID", []string{"export", "--state-dir", state, "../etc"}, 1, "形が正しくない"},
		{"sessions の余計な引数", []string{"sessions", "--state-dir", state, "x"}, exitUsage, "余計な引数"},
		{"sessions (空)", []string{"sessions", "--state-dir", state}, 0, "セッションは無い"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := dispatch(tc.args, &stdout, &stderr); code != tc.code {
				t.Errorf("終了コード = %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String()+stderr.String(), tc.want) {
				t.Errorf("出力に %q が無い\nstdout: %s\nstderr: %s", tc.want, stdout.String(), stderr.String())
			}
		})
	}
}

func TestResolveClaude(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "versions", "1.0")
	other := filepath.Join(dir, "other")
	notExec := filepath.Join(dir, "notexec")
	script := filepath.Join(dir, "wrapper")
	hashy := filepath.Join(dir, "hashy")     // # で始まるが、#! ではない (スクリプトではない)
	oneByte := filepath.Join(dir, "onebyte") // 1 バイトだけ (短いファイル)
	bang := filepath.Join(dir, "bang")       // 2 文字目が ! だが、1 文字目が # ではない
	for p, mode := range map[string]os.FileMode{real: 0o755, other: 0o755, notExec: 0o644, script: 0o755, hashy: 0o755, oneByte: 0o755, bang: 0o755} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "\x7fELF fake binary\n" // 実体の実行ファイルに見立てる (先頭が #! でないもの)
		switch p {
		case script:
			content = "#!/bin/sh\nexec /opt/claude-code/bin/claude \"$@\"\n"
		case hashy:
			content = "# not a shebang\n"
		case oneByte:
			content = "#"
		case bang:
			content = "x!not a shebang\n"
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	scriptLink := filepath.Join(dir, "bin", "claude-wrapper")
	if err := os.MkdirAll(filepath.Dir(scriptLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../wrapper", scriptLink); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../versions/1.0", link); err != nil {
		t.Fatal(err)
	}
	pathLookup := func(name string) (string, error) {
		if name != "claude" {
			t.Errorf("lookPath(%q)", name)
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
		{"PATH の claude は、symlink を辿った実体", "", "", pathLookup, real, ""},
		{"環境変数が PATH に勝つ", "", other, pathLookup, other, ""},
		{"--claude が環境変数に勝つ", link, other, pathLookup, real, ""},
		{"--claude は、symlink を辿る", link, "", noLookup, real, ""},
		{"PATH に無い", "", "", noLookup, "", "claude が見つからない"},
		{"実行できない", notExec, "", pathLookup, "", "実行できる通常のファイルではない"},
		{"ディレクトリ", dir, "", pathLookup, "", "実行できる通常のファイルではない"},
		{"存在しない", filepath.Join(dir, "none"), "", pathLookup, "", "claude"},
		{"# で始まるが #! ではない", hashy, "", pathLookup, hashy, ""},
		{"1 バイトのファイル", oneByte, "", pathLookup, oneByte, ""},
		{"2 文字目だけ !", bang, "", pathLookup, bang, ""},
		{"スクリプト (ラッパー)", script, "", pathLookup, "", "スクリプト"},
		{"スクリプトへの symlink", scriptLink, "", pathLookup, "", "スクリプト"},
		{"環境変数のスクリプト", "", script, pathLookup, "", "--claude か GORO_CLAUDE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveClaude(tc.flagVal, tc.env, tc.look)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveClaude = %q, %v, want error に %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveClaude = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

// testCage は、ホストの HOME が /home/u の、檻の設定。goldens の入力。
func testCage() cageConfig {
	return cageConfig{
		Host:      bwrap.Host{Home: "/home/u"},
		ClaudeExe: "/home/u/.local/share/claude/versions/2.0.0",
		GoroExe:   "/home/u/bin/goro",
		CACerts:   "/etc/ssl/certs",
		RunDir:    "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/run",
		AgentHome: "/home/u/.local/state/goro/home",
		Work:      "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/clone",
		Term:      "xterm-256color",
		Args:      []string{"--resume", "x y"},
	}
}

// 檻の argv (bwrap に渡す引数) を、丸ごと固定する。--ro-bind / / も --share-net も --new-session も無く、rw は 2 つだけで、
// 環境変数は許可リストだけ。
func TestCageSpecGolden(t *testing.T) {
	argv, err := cageSpec(testCage()).Argv()
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
		"--ro-bind", "/home/u/.local/share/claude/versions/2.0.0", "/opt/claude/claude",
		"--ro-bind", "/home/u/bin/goro", "/opt/goro/goro",
		"--ro-bind", "/home/u/.local/state/goro/sessions/20260926-120000-abcdef/run", "/run/goro",
		"--bind", "/home/u/.local/state/goro/home", "/home/goro",
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
	if !slices.Equal(argv, want) {
		t.Errorf("argv が golden と違う:\n got: %q\nwant: %q", argv, want)
	}
}

// HOME の下の Src には InHome を明示し、HOME の外の Src には付けない。rw なのは、エージェントの HOME と /work だけ。
func TestCageSpecBinds(t *testing.T) {
	spec := cageSpec(testCage())
	type bind struct{ rw, inHome bool }
	got := map[string]bind{}
	for _, b := range spec.Binds {
		got[b.Dst] = bind{b.RW, b.InHome}
	}
	want := map[string]bind{
		"/usr": {false, false}, "/etc/ssl/certs": {false, false},
		"/opt/claude/claude": {false, true}, "/opt/goro/goro": {false, true}, "/run/goro": {false, true},
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
	if spec.NewSession || spec.Chdir != "/work" || !slices.Equal(spec.Tmpfs, []string{"/tmp"}) {
		t.Errorf("NewSession = %v, Chdir = %q, Tmpfs = %v", spec.NewSession, spec.Chdir, spec.Tmpfs)
	}

	// HOME の外 (テストバイナリや /opt の claude) は、InHome が付かず、Argv も通る。CA が無ければ、その bind は無い。
	c := testCage()
	c.ClaudeExe, c.GoroExe, c.CACerts = "/opt/claude-real/claude", "/usr/local/bin/goro", ""
	spec = cageSpec(c)
	if _, err := spec.Argv(); err != nil {
		t.Fatalf("HOME の外の claude で Argv が error: %v", err)
	}
	for _, b := range spec.Binds {
		if b.Dst == "/etc/ssl/certs" {
			t.Error("CACerts が空なのに、/etc/ssl/certs を bind している")
		}
		if (b.Dst == "/opt/claude/claude" || b.Dst == "/opt/goro/goro") && b.InHome {
			t.Errorf("HOME の外の %s に InHome が付いている", b.Dst)
		}
	}
	// 機密の path にある claude・goro は、bwrap が拒否する (goro run が、それを回避しない)。
	c = testCage()
	c.ClaudeExe = "/home/u/.claude/local/claude"
	if _, err := cageSpec(c).Argv(); err == nil {
		t.Error("~/.claude の下の claude を、bwrap が拒否しない")
	}
	c = testCage()
	c.AgentHome = "/home/u/.ssh"
	if _, err := cageSpec(c).Argv(); err == nil {
		t.Error("~/.ssh を、エージェントの HOME として、bwrap が拒否しない")
	}
}

func TestCageEnv(t *testing.T) {
	names := func(env []bwrap.EnvVar) string {
		var out []string
		for _, e := range env {
			out = append(out, e.Key)
		}
		return strings.Join(out, ",")
	}
	wantNames := "HOME,PATH,TERM,LANG,CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC,DISABLE_TELEMETRY,DISABLE_ERROR_REPORTING,DISABLE_AUTOUPDATER,CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL"
	for term, want := range map[string]string{
		"xterm-256color": "xterm-256color", "screen.linux": "screen.linux", "": "dumb", "bad term": "dumb",
		"x\ny": "dumb", "$(id)": "dumb", strings.Repeat("a", 65): "dumb", "tmux-256color": "tmux-256color",
	} {
		env := cageEnv(term)
		if names(env) != wantNames {
			t.Fatalf("環境変数の名前 = %s, want %s", names(env), wantNames)
		}
		for _, e := range env {
			if e.Key == "TERM" && e.Value != want {
				t.Errorf("TERM=%q → %q, want %q", term, e.Value, want)
			}
		}
	}
	// 許可リストは、bwrap の資格情報らしい名前の検査に通る。
	if _, err := cageSpec(testCage()).Argv(); err != nil {
		t.Error(err)
	}
}

func TestCageArgs(t *testing.T) {
	if got := cageArgs(runOptions{login: true}); len(got) != 0 {
		t.Errorf("--login = %v, want 引数なし (素の対話起動。claude auth login は、onboarding の完了を保存しない)", got)
	}
	if got := cageArgs(runOptions{login: true, claudeArgs: []string{"--extra"}}); !slices.Equal(got, []string{"--extra"}) {
		t.Errorf("--login と引数 = %v", got)
	}
	if got := cageArgs(runOptions{repo: "r", claudeArgs: []string{"-p", "hi"}}); !slices.Equal(got, []string{"-p", "hi"}) {
		t.Errorf("--repo と引数 = %v", got)
	}
}

// egress の UDS の path が長すぎるときは、セッションの clone を作る前に断る (何も作らない: 孤児のセッションを残さない)。
// 検査する path は、上限ちょうどまで通り、1 バイト超えると断る。--login は、別の (短い) path を使う。
func TestRunRejectsLongSockPathBeforeCreate(t *testing.T) {
	for name, o := range map[string]runOptions{"--repo": {repo: "x"}, "--session": {session: "x"}, "--login": {login: true}} {
		probe := sockPathFor("/x", o) // 状態ディレクトリ /x の分 (2 バイト) を除いた長さで、上限に合わせる
		stateAt := func(total int) string { return "/" + strings.Repeat("a", total-len(probe)+1) }
		if err := checkSockPath(sockPathFor(stateAt(maxSockPath), o)); err != nil {
			t.Errorf("%s: 上限ちょうど (%d バイト) が通らない: %v", name, maxSockPath, err)
		}
		if err := checkSockPath(sockPathFor(stateAt(maxSockPath+1), o)); err == nil {
			t.Errorf("%s: 上限を 1 バイト超えても、通る", name)
		}
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	long := filepath.Join(shortDir(t), strings.Repeat("s", 90))
	for _, args := range [][]string{{"--repo", repo}, {"--session", "20260101-000000-aaaaaa"}, {"--login"}} {
		var stderr bytes.Buffer
		code := runRun(append([]string{"--state-dir", long, "--claude", self}, args...), &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "--state-dir") || !strings.Contains(stderr.String(), "長すぎる") {
			t.Errorf("%v: 終了コード = %d, stderr:\n%s", args, code, stderr.String())
		}
		if strings.Contains(stderr.String(), "セッション") || strings.Contains(stderr.String(), "--session") {
			t.Errorf("%v: 何も作らずに断るはずが、セッションの案内が出ている:\n%s", args, stderr.String())
		}
		if _, err := os.Stat(long); err == nil {
			t.Fatalf("%v: 断ったのに、状態ディレクトリ %s を作っている", args, long)
		}
	}
}

func TestCheckSockPath(t *testing.T) {
	ok := "/" + strings.Repeat("a", maxSockPath-1)
	if err := checkSockPath(ok); err != nil {
		t.Errorf("%d バイト: %v (上限ちょうどは通る)", len(ok), err)
	}
	long := ok + "a"
	err := checkSockPath(long)
	if err == nil {
		t.Fatalf("%d バイトが通る", len(long))
	}
	for _, want := range []string{"108 バイト", "107", "--state-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error に %q が無い: %v", want, err)
		}
	}
	// 上限ちょうどの UDS に、実際に待ち受けられる (上限の値が、カーネルの上限と合っている)。
	dir := shortDir(t)
	pad := maxSockPath - len(dir) - 1
	sock := filepath.Join(dir, strings.Repeat("s", pad))
	if len(sock) != maxSockPath {
		t.Fatalf("テストの path の長さ = %d", len(sock))
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("%d バイトの UDS に待ち受けられない: %v", len(sock), err)
	}
	l.Close()
	if _, err := net.Listen("unix", sock+"x"); err == nil {
		t.Error("108 バイトの UDS に待ち受けられてしまった (上限の前提が違う)")
	}
	// startProxy は、長すぎる run dir を、何も作らずに断る。
	longRun := filepath.Join(dir, strings.Repeat("d", 100))
	if _, err := startProxy(longRun, allowList(nil)); err == nil || !strings.Contains(err.Error(), "--state-dir") {
		t.Errorf("startProxy(長い path) = %v", err)
	}
	if _, err := os.Stat(longRun); err == nil {
		t.Error("長い path の run dir を作った")
	}
}

// connectVia は、UDS の egress に、CONNECT target を送り、応答の状態コードを返す。
func connectVia(t *testing.T, sock, target string) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(testTimeout))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("%s の応答を読めない: %v", target, err)
	}
	f := strings.Fields(string(buf[:n]))
	if len(f) < 2 {
		t.Fatalf("応答が正しくない: %q", buf[:n])
	}
	return f[1]
}

// startProxy: run dir の UDS (0600) で待ち受け、許可外は 403 にして監査に残し、Close で UDS を消す。--allow で足した宛先は、
// 403 にならない。同じ run dir を同時に使うことは断り、前の起動が残した UDS は取り替える。
func TestStartProxy(t *testing.T) {
	run := shortDir(t)
	stale, err := net.Listen("unix", filepath.Join(run, proxySockName)) // 前の起動が、消さずに残した UDS
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if err := os.WriteFile(filepath.Join(run, egressLogName), []byte(`{"event":"deny","reason":"not-allowed","target":"old.example:443"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := startProxy(run, allowList([]string{"extra.invalid:443"}))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p.sockPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("UDS の権限 = %v, %v, want 0600", fi, err)
	}
	if _, err := startProxy(run, allowList(nil)); err == nil || !strings.Contains(err.Error(), "すでに動いている") {
		t.Errorf("同じ run dir の 2 つ目の startProxy = %v, want すでに動いている", err)
	}
	if got := connectVia(t, p.sockPath, "denied.example:443"); got != "403" {
		t.Errorf("許可外の宛先 = %s, want 403", got)
	}
	if got := connectVia(t, p.sockPath, "extra.invalid:443"); got == "403" {
		t.Errorf("--allow で足した宛先が 403")
	}
	if got := connectVia(t, p.sockPath, "127.0.0.1:22"); got[0] != '4' {
		t.Errorf("IP リテラル = %s, want 4xx", got)
	}
	dropped, serveErr := p.Close()
	if dropped != 0 || serveErr != nil {
		t.Errorf("Close = %d, %v", dropped, serveErr)
	}
	if _, err := os.Lstat(p.sockPath); err == nil {
		t.Error("Close の後も、UDS が残っている")
	}
	top, more, err := deniedTargets(p.logPath, p.logStart, 10)
	if err != nil || more != 0 || len(top) != 1 || top[0] != (deniedTarget{"denied.example:443", 1}) {
		t.Errorf("deniedTargets = %v, %d, %v (前の起動の old.example は含まない。--allow の宛先は、拒否ではない)", top, more, err)
	}
	log, _ := os.ReadFile(p.logPath)
	if !strings.Contains(string(log), `"target":"denied.example:443"`) || !strings.Contains(string(log), `"event":"deny"`) {
		t.Errorf("監査ログに拒否が無い:\n%s", log)
	}

	// Close の後は、ロックが離れていて、もう一度起動できる。
	p2, err := startProxy(run, allowList(nil))
	if err != nil {
		t.Fatalf("Close の後の startProxy: %v", err)
	}
	p2.Close()
}

// blockingWriter は、release が閉じるまで、Write が戻らない (詰まったディスクの代わり)。
type blockingWriter struct{ release chan struct{} }

func (w blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// 監査の書き込みが詰まっても、egress の accept と Close は止まらない (Audit.Write が待つと、accept の中の拒否と Close が止まる)。
func TestAuditLogNeverBlocksEgress(t *testing.T) {
	old := auditCloseWait
	auditCloseWait = 200 * time.Millisecond
	defer func() { auditCloseWait = old }()

	w := blockingWriter{release: make(chan struct{})}
	defer close(w.release)
	audit := newAuditLog(w, 4, 1<<20)
	srv := egress.New(egress.Config{Allow: allowList(nil), Audit: audit, MaxConns: 2})
	sock := tempSock(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()

	const n = 40
	for i := 0; i < n; i++ {
		if got := connectVia(t, sock, "denied.example:443"); got != "403" {
			t.Fatalf("%d 回目: %s, want 403 (監査が詰まっても、応答は返る)", i, got)
		}
	}
	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(testTimeout):
		t.Fatal("監査が詰まると、egress の Close が返らない")
	}
	if err := <-served; !errors.Is(err, egress.ErrClosed) {
		t.Errorf("Serve = %v", err)
	}
	start := time.Now()
	dropped := audit.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("auditLog.Close が %v かかった (待つ上限は %v)", d, auditCloseWait)
	}
	if dropped < n-8 {
		t.Errorf("捨てた行 = %d, want %d 以上 (ためられる 4 行と、書き込み中の 1 行の他は捨てる)", dropped, n-8)
	}
}

func TestAuditLogBudgetAndErrors(t *testing.T) {
	var buf bytes.Buffer
	a := newAuditLog(&buf, 16, 30)
	for i := 0; i < 5; i++ {
		if n, err := a.Write([]byte("0123456789")); n != 10 || err != nil {
			t.Fatalf("Write = %d, %v (常に成功を返す)", n, err)
		}
	}
	if dropped := a.Close(); dropped != 2 || buf.Len() != 30 {
		t.Errorf("書き込みの上限 30 バイトで、捨てた行 = %d (want 2)、書いた量 = %d (want 30)", dropped, buf.Len())
	}
	// Close の後の Write は、捨てる (panic しない)。
	if n, err := a.Write([]byte("late")); n != 4 || err != nil {
		t.Errorf("Close の後の Write = %d, %v", n, err)
	}
	if a.Close() != 2 {
		t.Error("Close を 2 回呼ぶと、結果が変わる")
	}

	// 書き込みの失敗も、数える。
	b := newAuditLog(errWriter{}, 4, 1<<20)
	b.Write([]byte("x"))
	b.Write([]byte("y"))
	if dropped := b.Close(); dropped != 2 {
		t.Errorf("書き込みに失敗した行 = %d, want 2", dropped)
	}

	// 呼び手が渡した slice を、後から書き換えても、ためた行は変わらない (egress は、行ごとに新しい slice を渡すが、写しを持つ)。
	var out bytes.Buffer
	c := newAuditLog(&out, 4, 1<<20)
	p := []byte("abc\n")
	c.Write(p)
	copy(p, "XYZ")
	c.Close()
	if out.String() != "abc\n" {
		t.Errorf("書かれた行 = %q, want abc", out.String())
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestParseDenied(t *testing.T) {
	var log strings.Builder
	add := func(s string) { log.WriteString(s + "\n") }
	deny := func(target string) string {
		return fmt.Sprintf(`{"time":"t","event":"deny","conn":1,"target":%q,"reason":"not-allowed","status":403}`, target)
	}
	add(deny("b.example:443"))
	add(deny("a.example:443"))
	add(deny("b.example:443"))
	add(`{"event":"deny","reason":"forbidden-ip","target":"c.example:443"}`) // --allow で直せない拒否
	add(`{"event":"error","reason":"not-allowed","target":"d.example:443"}`)
	add(`{"event":"allow","reason":"allowlist","target":"e.example:443"}`)
	add(`{"event":"deny","reason":"not-allowed"}`) // 宛先が無い
	add(`not json`)
	add(`{"event":"deny","reason":"not-allowed","target":"` + strings.Repeat("x", 5000) + `"}`) // 長すぎる行
	add(deny("a.example:443"))
	add(deny("a.example:443"))
	log.WriteString(deny("z.example:443")) // 改行の無い最後の行

	top, more, err := parseDenied(strings.NewReader(log.String()), 10)
	if err != nil || more != 0 {
		t.Fatalf("parseDenied = %v, %d, %v", top, more, err)
	}
	want := []deniedTarget{{"a.example:443", 3}, {"b.example:443", 2}, {"z.example:443", 1}}
	if !slices.Equal(top, want) {
		t.Errorf("top = %v, want %v (回数の多い順)", top, want)
	}

	// 上限を超えると、上位だけと、残りの件数。同じ回数は、初めて出た順。
	log.Reset()
	for i := 0; i < 13; i++ {
		add(deny(fmt.Sprintf("h%02d.example:443", i)))
	}
	add(deny("h12.example:443"))
	top, more, err = parseDenied(strings.NewReader(log.String()), 10)
	if err != nil || more != 3 || len(top) != 10 || top[0] != (deniedTarget{"h12.example:443", 2}) || top[1].Target != "h00.example:443" || top[9].Target != "h08.example:443" {
		t.Errorf("parseDenied (13 件) = %v, %d, %v", top, more, err)
	}
}

func TestPrintRunSummary(t *testing.T) {
	var w bytes.Buffer
	printRunSummary(&w, runSummary{
		id: "20260926-120000-abcdef", started: true, stateDir: "/tmp/s t'x", stateDirGiven: true, agentHome: "/tmp/s t'x/home", logPath: "/tmp/s/run/egress.log",
		denied: []deniedTarget{{"cdn.example:443", 3}, {"evil\x1b[31m.example:443", 1}}, deniedMore: 4, dropped: 2, serveErr: errors.New("boom"),
	})
	got := w.String()
	for _, want := range []string{
		"セッション: 20260926-120000-abcdef",
		`goro run --state-dir '/tmp/s t'\''x' --session 20260926-120000-abcdef`,
		`goro export --state-dir '/tmp/s t'\''x' 20260926-120000-abcdef`,
		"egress の監査ログ: /tmp/s/run/egress.log",
		"最大 10 件",
		"  cdn.example:443 (3 回)",
		"  evil?[31m.example:443 (1 回)", // 制御文字は ? にする
		"ほか 4 件",
		"--allow cdn.example:443",
		"2 行、書けずに捨てた",
		"egress の待ち受けが異常に終わった: boom",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("案内に %q が無い:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("案内に生の制御文字がある: %q", got)
	}

	// 既定の state dir と、拒否が無いとき: 余計なものを出さない。
	w.Reset()
	printRunSummary(&w, runSummary{id: "20260926-120000-abcdef", started: true, stateDir: "/x", logPath: "/x/log"})
	got = w.String()
	if strings.Contains(got, "--state-dir") || strings.Contains(got, "拒否") || strings.Contains(got, "注意") || !strings.Contains(got, "goro run --session 20260926-120000-abcdef") {
		t.Errorf("既定の案内:\n%s", got)
	}

	// --login
	w.Reset()
	printRunSummary(&w, runSummary{stateDir: "/x", agentHome: "/x/home", started: true, logPath: "/x/login-run/egress.log"})
	got = w.String()
	if !strings.Contains(got, "檻専用の HOME (ログイン状態が残る): /x/home") || !strings.Contains(got, "goro run --repo PATH") || strings.Contains(got, "セッション:") {
		t.Errorf("--login の案内:\n%s", got)
	}
	// 檻を起動できなかったなら、何も案内しない (必ず失敗する再開・中身の無い取り出しを、案内しない)。--login も、セッションも。
	for _, sum := range []runSummary{
		{stateDir: "/x", agentHome: "/x/home", logPath: "/x/login-run/egress.log"},
		{id: "20260926-120000-abcdef", stateDir: "/x", logPath: "/x/log", denied: []deniedTarget{{"a.example:443", 1}}, dropped: 3},
	} {
		w.Reset()
		printRunSummary(&w, sum)
		if w.Len() != 0 {
			t.Errorf("檻を起動できなかったのに、案内を出している (%+v):\n%s", sum, w.String())
		}
	}
}

func TestPrintSessionsAndBundle(t *testing.T) {
	var w bytes.Buffer
	printSessions(&w, nil)
	if w.String() != "セッションは無い\n" {
		t.Errorf("空の一覧 = %q", w.String())
	}
	w.Reset()
	created := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	printSessions(&w, []session.Info{{ID: "20260926-120000-abcdef", Created: created, Repo: "foo"}, {ID: "20260926-130000-bbbbbb", Created: created.Add(time.Hour)}})
	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "20260926-120000-abcdef  ") || !strings.HasSuffix(lines[0], "  foo") ||
		!strings.HasSuffix(lines[1], "  -") || !strings.Contains(lines[0], created.Local().Format("2006-01-02 15:04:05")) {
		t.Errorf("一覧 = %q", lines)
	}

	w.Reset()
	printBundle(&w, &session.Bundle{Path: "/s/export/goro.bundle", Size: 1234, Heads: []string{"refs/heads/main", "refs/heads/x"}, Skipped: 1,
		Fetch: "git -c transfer.fsckObjects=true fetch '/s/export/goro.bundle' 'refs/heads/main:refs/heads/goro/ID/main'"})
	for _, want := range []string{"bundle: /s/export/goro.bundle (1234 バイト)", "ブランチ: refs/heads/main, refs/heads/x", "除いたブランチ: 1",
		"自分の repo で実行する", "  git -c transfer.fsckObjects=true fetch '/s/export/goro.bundle'"} {
		if !strings.Contains(w.String(), want) {
			t.Errorf("bundle の表示に %q が無い:\n%s", want, w.String())
		}
	}
}

func TestExitCodeOf(t *testing.T) {
	run := func(script string) error { return exec.Command("/bin/sh", "-c", script).Run() }
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"成功", nil, 0},
		{"終了コード 7", run("exit 7"), 7},
		{"SIGKILL", run("kill -9 $$"), 128 + 9},
		{"SIGTERM", run("kill -TERM $$"), 128 + 15},
		{"exec 以外の error", errors.New("x"), 1},
	} {
		if got := exitCodeOf(tc.err, io.Discard); got != tc.want {
			t.Errorf("%s: exitCodeOf = %d, want %d", tc.name, got, tc.want)
		}
	}
	if exitCodeForSignal(syscall.SIGTERM) != 143 || exitCodeForSignal(syscall.SIGHUP) != 129 || exitCodeForSignal(syscall.SIGINT) != 130 {
		t.Error("exitCodeForSignal が、128 + 番号ではない")
	}
}

// waitFor は、cond が真になるのを、1 秒まで待つ。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s が起きない", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 檻を起動する前は、SIGINT・SIGTERM・SIGHUP のどれでも取り消す。檻に入った後 (enterCage) は、SIGINT と SIGQUIT を無視し
// (ホストの goro run が、claude の中断で落ちない)、SIGTERM と SIGHUP だけで取り消す。stop は、元に戻す。
func TestSigWatch(t *testing.T) {
	self := syscall.Getpid()
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		ctx, cancel := context.WithCancel(context.Background())
		sw := watchSignals(cancel)
		if err := syscall.Kill(self, sig); err != nil {
			t.Fatal(err)
		}
		waitFor(t, fmt.Sprintf("%v での取り消し", sig), func() bool { return ctx.Err() != nil })
		if sw.received() != sig || exitCodeForSignal(sw.received()) != 128+int(sig) {
			t.Errorf("received = %v, want %v", sw.received(), sig)
		}
		sw.stop()
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := watchSignals(cancel)
	sw.enterCage()
	if !signal.Ignored(syscall.SIGINT) || !signal.Ignored(syscall.SIGQUIT) {
		t.Fatal("enterCage の後も、SIGINT・SIGQUIT を無視していない")
	}
	if signal.Ignored(syscall.SIGTERM) || signal.Ignored(syscall.SIGHUP) {
		t.Error("SIGTERM・SIGHUP を無視している (これは、檻を止めるために受ける)")
	}
	// 無視している SIGINT・SIGQUIT を、自分に送っても、落ちず、取り消しにならない (ホストの goro run に届く、Ctrl-C の代わり)。
	syscall.Kill(self, syscall.SIGINT)
	syscall.Kill(self, syscall.SIGQUIT)
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil || sw.received() != nil {
		t.Fatalf("檻の中で、SIGINT・SIGQUIT で取り消された (%v)", sw.received())
	}
	syscall.Kill(self, syscall.SIGTERM)
	waitFor(t, "檻の中での SIGTERM での取り消し", func() bool { return ctx.Err() != nil })
	if sw.received() != syscall.SIGTERM {
		t.Errorf("received = %v, want SIGTERM", sw.received())
	}
	sw.stop()
	if signal.Ignored(syscall.SIGINT) || signal.Ignored(syscall.SIGQUIT) {
		t.Error("stop の後も、SIGINT・SIGQUIT を無視している")
	}
}

// 檻に入る前に届いたまま、ためられていた SIGINT は、檻に入った後では、取り消しにしない。
func TestSigWatchLateSigint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &sigWatch{ch: make(chan os.Signal, 4), cancel: cancel, done: make(chan struct{})}
	w.inCage.Store(true)
	w.ch <- syscall.SIGINT
	go w.loop()
	defer close(w.done)
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil {
		t.Error("檻に入った後で、ためられていた SIGINT が、取り消しになった")
	}
	w.ch <- syscall.SIGTERM
	waitFor(t, "SIGTERM での取り消し", func() bool { return ctx.Err() != nil })
}

// 検査だけ (bwrap が要らない) の、起動前の失敗: 存在しないセッション・見つからない claude は、檻を起動せず、終了コード 1。
func TestRunFailsBeforeCage(t *testing.T) {
	state := shortDir(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"存在しないセッション", []string{"--state-dir", state, "--claude", self, "--session", "20260101-000000-aaaaaa"}, "セッションを使えない"},
		{"形が違うセッション ID", []string{"--state-dir", state, "--claude", self, "--session", "../x"}, "セッションを使えない"},
		{"claude が無い", []string{"--state-dir", state, "--claude", filepath.Join(state, "none"), "--login"}, "claude"},
		{"repo が無い", []string{"--state-dir", state, "--claude", self, "--repo", filepath.Join(state, "none")}, "セッションを作れない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORO_CLAUDE", "")
			var stderr bytes.Buffer
			if code := runRun(tc.args, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("終了コード = %d (want 1), stderr:\n%s\nwant に %q", code, stderr.String(), tc.want)
			}
		})
	}
	if ents, _ := os.ReadDir(filepath.Join(state, "sessions")); len(ents) != 0 {
		t.Errorf("失敗したのに、セッションが残った: %v", ents)
	}
}

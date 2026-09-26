package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 結合テスト (bwrap が要る): 実プロセスの goro run (テストバイナリを、argv[0] = goro で再実行したもの) が、偽の claude
// (テストバイナリを、argv[0] = claude で再実行したもの。GORO_CLAUDE で指す) を、本物の檻の中で動かす。
// goro run は新しいセッション (Setsid) で起動する: 制御端末を持たず (TIOCSTI の確認が、開発者の端末に左右されない)、
// プロセスグループが自分だけになる (端末のシグナルを、グループへの kill で真似られる)。

const (
	gitPath    = "/usr/bin/git"
	runTimeout = 90 * time.Second
)

// runFixture は、偽の HOME・元の repo・goro run の環境変数。
type runFixture struct {
	dir  string // 短い path の作業ディレクトリ
	home string // 偽のホストの HOME (~/.ssh などの目印がある)
	repo string // 元の repo
	exe  string // テストバイナリ (goro としても、偽の claude としても、動く)
	env  []string
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	requireBwrap(t)
	if _, err := os.Stat(gitPath); err != nil {
		t.Skipf("%s が無い: %v", gitPath, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDir(t)
	f := &runFixture{dir: dir, home: filepath.Join(dir, "home"), repo: filepath.Join(dir, "repo"), exe: exe}
	for path, content := range map[string]string{
		filepath.Join(f.home, ".ssh", "id_test"):                          "PRIVATE-KEY-MARKER\n",
		filepath.Join(f.home, ".claude", "credentials.json"):              "TOKEN-MARKER\n",
		filepath.Join(f.home, ".local", "share", "opencode", "auth.json"): "OPENCODE-AUTH-MARKER\n",
		filepath.Join(f.home, ".config", "opencode", "opencode.json"):     "OPENCODE-CONFIG-MARKER\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.env = []string{"HOME=" + f.home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "TERM=xterm-256color", "GORO_CLAUDE=" + exe, "GORO_OPENCODE=" + exe}
	if err := os.Mkdir(f.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.repo, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("# repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.repo, "add", ".")
	f.git(t, f.repo, "commit", "-q", "-m", "initial")
	return f
}

// git は、ホストで git を実行する (テストの準備と、利用者の git fetch を模す確認だけ。goro 本体は、ホストで git を実行しない)。
func (f *runFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitPath, append([]string{"-c", "user.name=t", "-c", "user.email=t@e.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + f.dir, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// syncBuffer は、複数の goroutine から書ける bytes.Buffer。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// liveRun は、動いている goro の子プロセス。
type liveRun struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout syncBuffer
	stderr syncBuffer
	done   chan struct{}
	err    error
}

// start は、goro を、args で、新しいセッションで起動する。
func (f *runFixture) start(t *testing.T, args ...string) *liveRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	t.Cleanup(cancel)
	r := &liveRun{t: t, done: make(chan struct{})}
	r.cmd = exec.CommandContext(ctx, f.exe, args...)
	r.cmd.Args[0] = "goro"
	r.cmd.Env = f.env
	r.cmd.Stdout, r.cmd.Stderr = &r.stdout, &r.stderr
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		r.err = r.cmd.Wait() // 檻の中の全員が、標準出力を閉じるまで、戻らない
		close(r.done)
	}()
	return r
}

// waitStdout は、標準出力に substr が出るのを待つ。
func (r *liveRun) waitStdout(substr string) {
	r.t.Helper()
	deadline := time.After(runTimeout)
	for !strings.Contains(r.stdout.String(), substr) {
		select {
		case <-r.done:
			if strings.Contains(r.stdout.String(), substr) {
				return
			}
			r.t.Fatalf("%q が出る前に、goro が終わった: %v\nstdout:\n%s\nstderr:\n%s", substr, r.err, r.stdout.String(), r.stderr.String())
		case <-deadline:
			r.t.Fatalf("%q が出ない\nstdout:\n%s\nstderr:\n%s", substr, r.stdout.String(), r.stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// wait は、goro の終了を待ち、終了コード (シグナルで死んだら -1) を返す。
func (r *liveRun) wait() int {
	r.t.Helper()
	select {
	case <-r.done:
	case <-time.After(runTimeout):
		r.t.Fatalf("goro が終わらない (檻の中に、標準出力を持ったまま残っているものがあるか)\nstdout:\n%s\nstderr:\n%s", r.stdout.String(), r.stderr.String())
	}
	if r.err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(r.err, &ee) {
		return ee.ExitCode()
	}
	r.t.Fatalf("goro の Wait: %v", r.err)
	return -1
}

// goroResult は、終わった goro の結果。
type goroResult struct {
	stdout, stderr string
	code           int
}

// goro は、goro を args で起動し、終わるのを待つ。
func (f *runFixture) goro(t *testing.T, args ...string) goroResult {
	t.Helper()
	r := f.start(t, args...)
	code := r.wait()
	return goroResult{r.stdout.String(), r.stderr.String(), code}
}

func (r goroResult) String() string {
	return fmt.Sprintf("終了コード %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

// mustOK は、終了コードが 0 でなければ、テストを止める。
func (r goroResult) mustOK(t *testing.T) goroResult {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("goro が失敗した: %s", r)
	}
	return r
}

// parseOut は、偽の claude の出力を、"キー=値" と "操作 => 結果" に分ける。
func parseOut(out string) (kv, arrow map[string]string) {
	kv, arrow = map[string]string{}, map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if op, res, ok := strings.Cut(l, " => "); ok {
			arrow[op] = res
		} else if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = v
		}
	}
	return kv, arrow
}

var sessionIDRE = regexp.MustCompile(`セッション: (\d{8}-\d{6}-[0-9a-f]{6})`)

// sessionID は、goro run の案内から、セッション ID を取り出す。
func sessionID(t *testing.T, r goroResult) string {
	t.Helper()
	m := sessionIDRE.FindStringSubmatch(r.stderr)
	if m == nil {
		t.Fatalf("案内にセッション ID が無い: %s", r)
	}
	return m[1]
}

func (f *runFixture) stateDir() string { return filepath.Join(f.home, ".local", "state", "goro") }

// (a) 偽の claude が /work に commit する → goro export → bundle → 別の repo で、表示された取り込みのコマンドを実行できる。
// (d) --session で再開すると、同じ clone が見える。sessions は、一覧に出す。
func TestRunCommitExportFetchResume(t *testing.T) {
	f := newRunFixture(t)

	run1 := f.goro(t, "run", "--repo", f.repo, "--", "commit", "hello.txt", "hi there", "add hello").mustOK(t)
	kv, _ := parseOut(run1.stdout)
	commit := strings.TrimSpace(kv["commit"])
	if len(commit) != 40 {
		t.Fatalf("偽の claude が commit できていない: %s", run1)
	}
	id := sessionID(t, run1)
	for _, want := range []string{"goro run --session " + id, "goro export " + id} {
		if !strings.Contains(run1.stderr, want) {
			t.Errorf("終了後の案内に %q が無い:\n%s", want, run1.stderr)
		}
	}
	if want, got := filepath.Join(f.stateDir(), "sessions", id, "run", proxySockName), sockPathFor(f.stateDir(), runOptions{repo: f.repo}); len(got) != len(want) {
		t.Errorf("Create の前に調べる UDS の path (%d バイト) が、実際の path (%d バイト) と違う: session の ID の形が変わった", len(got), len(want))
	}
	if _, err := os.Stat(filepath.Join(f.repo, "hello.txt")); err == nil {
		t.Error("ホストの元の repo に、檻の中の変更が入っている")
	}

	// export → 別の repo に取り込む。
	exp := f.goro(t, "export", id).mustOK(t)
	if !strings.Contains(exp.stdout, "refs/heads/main") || !strings.Contains(exp.stdout, "goro.bundle") {
		t.Fatalf("export の表示:\n%s", exp)
	}
	var fetch string
	lines := strings.Split(exp.stdout, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "取り込み") && i+1 < len(lines) {
			fetch = strings.TrimSpace(lines[i+1])
		}
	}
	if !strings.HasPrefix(fetch, "git -c transfer.fsckObjects=true fetch ") {
		t.Fatalf("取り込みのコマンドが表示されていない: %q\n%s", fetch, exp.stdout)
	}
	dest := filepath.Join(f.dir, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(t, dest, "init", "-q", "-b", "main", ".")
	cmd := exec.Command("/bin/sh", "-c", fetch)
	cmd.Dir = dest
	cmd.Env = []string{"HOME=" + f.dir, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("表示された取り込みのコマンドが失敗した: %v\n%s\n%s", err, fetch, out)
	}
	ref := "refs/heads/goro/" + id + "/main"
	if got := strings.TrimSpace(f.git(t, dest, "rev-parse", ref)); got != commit {
		t.Errorf("取り込んだ %s = %s, want %s", ref, got, commit)
	}
	if got := f.git(t, dest, "show", ref+":hello.txt"); got != "hi there" {
		t.Errorf("取り込んだ hello.txt = %q", got)
	}

	// sessions
	ss := f.goro(t, "sessions").mustOK(t)
	if !strings.Contains(ss.stdout, id) || !strings.HasSuffix(strings.TrimSpace(ss.stdout), "  repo") {
		t.Errorf("sessions の表示 (ID と、元の repo 名 repo が出る):\n%s", ss.stdout)
	}

	// (d) 再開: 前の commit と、ファイルが残っている。
	run2 := f.goro(t, "run", "--session", id, "--", "gitlog", "hello.txt").mustOK(t)
	if !strings.Contains(run2.stdout, "log="+commit) || !strings.Contains(run2.stdout, `file="hi there"`) {
		t.Errorf("再開した clone に、前の commit が見えない:\n%s", run2)
	}
	if got := sessionIDRE.FindStringSubmatch(run2.stderr); got == nil || got[1] != id {
		t.Errorf("再開の案内のセッション ID が違う:\n%s", run2.stderr)
	}
	if strings.Contains(run2.stderr, "を作った") {
		t.Errorf("再開なのに、セッションを作っている:\n%s", run2.stderr)
	}
}

// (b) egress: 偽の claude が、環境変数の HTTPS_PROXY へ CONNECT する。許可外は 403 で、監査に残り、終了後の案内に出る。
// 既定の宛先と、--allow で足した宛先は、403 にならない (200 か、CI にネットワークが無いときの 502・504)。
func TestRunEgressAllowList(t *testing.T) {
	f := newRunFixture(t)
	r := f.goro(t, "run", "--repo", f.repo, "--allow", "example.org:443", "--",
		"connect", "example.com:443", "example.com:80", "api.anthropic.com:443", "platform.claude.com:443", "example.org:443", "127.0.0.1:22").mustOK(t)
	_, res := parseOut(r.stdout)
	for _, target := range []string{"example.com:443", "example.com:80"} {
		if res[target] != "403" {
			t.Errorf("許可外の %s = %q, want 403", target, res[target])
		}
	}
	for _, target := range []string{"api.anthropic.com:443", "platform.claude.com:443", "example.org:443"} {
		if s := res[target]; s == "403" || strings.HasPrefix(s, "err") || s == "" {
			t.Errorf("許可した %s = %q, want 403 でも err でもない (200・502・504)", target, s)
		}
	}
	if s := res["127.0.0.1:22"]; !strings.HasPrefix(s, "4") {
		t.Errorf("IP リテラル = %q, want 4xx", s)
	}

	id := sessionID(t, r)
	log, err := os.ReadFile(filepath.Join(f.stateDir(), "sessions", id, "run", egressLogName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"deny"`, `"target":"example.com:443"`, `"target":"example.com:80"`, `"reason":"not-allowed"`} {
		if !strings.Contains(string(log), want) {
			t.Errorf("監査ログに %s が無い:\n%s", want, log)
		}
	}
	for _, l := range strings.Split(string(log), "\n") {
		for _, allowed := range []string{"example.org:443", "api.anthropic.com:443", "platform.claude.com:443"} {
			if strings.Contains(l, `"event":"deny"`) && strings.Contains(l, `"target":"`+allowed+`"`) {
				t.Errorf("許可した宛先が、拒否として記録されている: %s", l)
			}
		}
	}
	for _, want := range []string{"拒否された宛先", "example.com:443 (1 回)", "example.com:80 (1 回)", "--allow"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("終了後の案内に %q が無い:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stderr, "example.org:443 (") || strings.Contains(r.stderr, "api.anthropic.com:443 (") {
		t.Errorf("許可した宛先が、拒否の一覧に出ている:\n%s", r.stderr)
	}
	if _, err := os.Lstat(filepath.Join(f.stateDir(), "sessions", id, "run", proxySockName)); err == nil {
		t.Error("終了後も、egress の UDS が残っている")
	}
}

// (c) 檻の中: 外部・ホストの loopback に届かず、ホストの ~/.ssh・~/.claude・HOME・元の repo・状態ディレクトリが見えず、
// システムと実行ファイルと run dir は書けず、/work・檻専用の HOME・/tmp だけ書ける。環境変数は、許可リストだけ。
func TestRunCageIsolation(t *testing.T) {
	f := newRunFixture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0") // ホストの loopback で待ち受ける (檻の loopback とは別)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// 檻から見えてはいけないもの (ホストの HOME・鍵・認証情報・元の repo・状態ディレクトリ・作業ディレクトリ・実 HOME・/etc と /root)。
	hidden := []string{
		"stat:" + f.home, "stat:" + filepath.Join(f.home, ".ssh"), "stat:" + filepath.Join(f.home, ".ssh", "id_test"),
		"stat:" + filepath.Join(f.home, ".claude", "credentials.json"), "stat:" + f.repo, "stat:" + f.stateDir(), "stat:" + f.dir,
		"stat:/etc/passwd", "stat:/root", "stat:/home/" + filepath.Base(f.home),
	}
	// 届いてはいけないもの (外部・ホストの loopback)。
	unreachable := []string{"dial:1.1.1.1:443", "dial:" + l.Addr().String()}
	// ro の mount のはずのもの (ro の mount に、書き込みは失敗する)。
	readOnly := []string{"/usr", "/etc/ssl/certs", "/opt/claude/claude", "/opt/goro/goro", "/run/goro"}
	readOnlyWrites := []string{"write:/usr/x", "write:/etc/ssl/certs/x", "write:/run/goro/x", "write:/run/goro/proxy.sock", "write:/run/goro/egress.log"}
	// rw の mount のはずのもの。
	readWrite := []string{"/home/goro", "/work"}
	writable := []string{"write:/work/x", "write:/home/goro/x", "write:/tmp/x"}
	visible := []string{"stat:/opt/goro/goro", "stat:/opt/claude/claude", "stat:/usr/bin/git", "stat:/etc/ssl/certs", "stat:/run/goro/egress.log", "dial:127.0.0.1:3128"}

	var probes []string
	for _, g := range [][]string{hidden, unreachable, readOnlyWrites, writable, visible} {
		probes = append(probes, g...)
	}
	for _, p := range append(slices.Clone(readOnly), readWrite...) {
		probes = append(probes, "mnt:"+p)
	}
	r := f.goro(t, append([]string{"run", "--repo", f.repo, "--", "probe"}, probes...)...).mustOK(t)
	_, res := parseOut(r.stdout)
	for _, g := range []struct {
		what string
		ops  []string
		ok   bool
	}{
		{"ホストのものが、檻から見える", hidden, false},
		{"外部・ホストの loopback に、直接届く", unreachable, false},
		{"ro のはずの path に、書けた", readOnlyWrites, false},
		{"書けるはずの path に、書けない", writable, true},
		{"見えるはずの path・檻の loopback の中継 (goro init) に、届かない", visible, true},
	} {
		for _, op := range g.ops {
			if s := res[op]; (s == "ok") != g.ok || (!g.ok && !strings.HasPrefix(s, "err")) {
				t.Errorf("%s: %s = %q", g.what, op, s)
			}
		}
	}
	for _, p := range readOnly {
		if got := res["mnt:"+p]; got != "ro" {
			t.Errorf("%s の mount = %q, want ro", p, got)
		}
	}
	for _, p := range readWrite {
		if got := res["mnt:"+p]; got != "rw" {
			t.Errorf("%s の mount = %q, want rw", p, got)
		}
	}
	for _, secret := range []string{"PRIVATE-KEY-MARKER", "TOKEN-MARKER"} {
		if strings.Contains(r.stdout, secret) || strings.Contains(r.stderr, secret) {
			t.Errorf("出力に、ホストの秘密の中身 %s がある", secret)
		}
	}

	// 環境変数と、起動の状態。
	info := f.goro(t, "run", "--repo", f.repo, "--", "info").mustOK(t)
	kv, _ := parseOut(info.stdout)
	if kv["home"] != "/home/goro" || kv["cwd"] != "/work" || kv["https_proxy"] != "http://127.0.0.1:3128" || kv["no_proxy"] != "127.0.0.1,localhost,::1" {
		t.Errorf("HOME・cwd・HTTPS_PROXY・NO_PROXY = %q・%q・%q・%q", kv["home"], kv["cwd"], kv["https_proxy"], kv["no_proxy"])
	}
	allowed := map[string]bool{}
	for _, n := range []string{"HOME", "PATH", "TERM", "LANG", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_TELEMETRY",
		"DISABLE_ERROR_REPORTING", "DISABLE_AUTOUPDATER", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy", "PWD"} {
		allowed[n] = true
	}
	for _, n := range strings.Split(kv["env"], ",") {
		if !allowed[n] {
			t.Errorf("檻の環境変数に、許可リスト外の %s がある (%s)", n, kv["env"])
		}
	}
	for _, n := range []string{"HOME", "PATH", "TERM", "LANG", "DISABLE_TELEMETRY", "HTTPS_PROXY"} {
		if !strings.Contains(","+kv["env"]+",", ","+n+",") {
			t.Errorf("檻の環境変数に %s が無い (%s)", n, kv["env"])
		}
	}
	if !strings.Contains(kv["work"], "README.md") {
		t.Errorf("/work に clone が見えない: %q", kv["work"])
	}
}

// (e) --login: repo・clone 無しで、空の作業ディレクトリで、claude auth login の引数で起動する。ログイン状態は、檻専用の HOME に残り、
// 次の --repo の檻に見える。--state-dir は、案内に含まれる。
func TestRunLogin(t *testing.T) {
	f := newRunFixture(t)
	r := f.goro(t, "run", "--login", "--", "auth", "--extra").mustOK(t) // fake claude の場面 auth が、ログインの目印を作る
	kv, _ := parseOut(r.stdout)
	if kv["args"] != `["auth" "--extra"]` {
		t.Errorf("claude の引数 = %s, want [auth --extra] (--login は、claude auth login を付けない)", kv["args"])
	}
	if !strings.Contains(r.stderr, "ログインの画面が出ます。画面の指示に従い、終わったら終了してください (終了: /exit)。") ||
		strings.Contains(r.stderr, "Security notes") || strings.Contains(r.stderr, "テーマ") { // エージェントの画面の内容を、説明しない
		t.Errorf("--login の起動前の案内が無い:\n%s", r.stderr)
	}
	if kv["cwd"] != "/work" || kv["work"] != "" || kv["home"] != "/home/goro" {
		t.Errorf("cwd・/work の中身・HOME = %q・%q・%q (空の作業ディレクトリのはず)", kv["cwd"], kv["work"], kv["home"])
	}
	if b, err := os.ReadFile(f.agentPath("claude", "home", "login-marker")); err != nil || string(b) != "logged-in\n" {
		t.Errorf("ログイン状態が、檻専用の HOME (<state>/agents/claude/home) に残っていない: %q, %v", b, err)
	}
	if _, err := os.Stat(f.agentPath("claude", "login-run", egressLogName)); err != nil {
		t.Errorf("ログイン用の run dir に、監査ログが無い: %v", err)
	}
	if !strings.Contains(r.stderr, "ログイン状態: ") || !strings.Contains(r.stderr, "goro run --repo PATH") || strings.Contains(r.stderr, "セッション:") {
		t.Errorf("--login の案内:\n%s", r.stderr)
	}
	if ss := f.goro(t, "sessions").mustOK(t); !strings.Contains(ss.stdout, "セッションは無い") {
		t.Errorf("--login が、セッションを作った:\n%s", ss.stdout)
	}

	// 次の --repo の檻で、ログイン状態が見える。ホストの HOME の .claude は、見えない。
	next := f.goro(t, "run", "--repo", f.repo, "--", "marker").mustOK(t)
	if !strings.Contains(next.stdout, `marker="logged-in\n"`) {
		t.Errorf("ログイン状態が、次の檻に見えない:\n%s", next)
	}

	// --state-dir
	st := filepath.Join(f.dir, "st")
	r = f.goro(t, "run", "--state-dir", st, "--login", "--", "auth").mustOK(t)
	if _, err := os.Stat(filepath.Join(st, "agents", "claude", "home", "login-marker")); err != nil {
		t.Errorf("--state-dir の下の HOME に、ログイン状態が無い: %v", err)
	}
	if !strings.Contains(r.stderr, "goro run --state-dir '"+st+"' --repo PATH") {
		t.Errorf("案内に --state-dir が含まれない:\n%s", r.stderr)
	}
}

// 端末のシグナル: Ctrl-C (SIGINT)・SIGQUIT は、フォアグラウンドの process group 全体に届く。ホストの goro run は無視して落ちず
// (終了コードは claude のもの)、檻の中の claude は、ちょうど 1 回、直接受ける。無視の設定は、claude に引き継がれない。
func TestRunTerminalSignals(t *testing.T) {
	f := newRunFixture(t)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			r := f.start(t, "run", "--repo", f.repo, "--", "sigcount")
			r.waitStdout("ready")
			if err := syscall.Kill(-r.cmd.Process.Pid, sig); err != nil { // 端末が、フォアグラウンドのグループに送るのと同じ
				t.Fatal(err)
			}
			if code := r.wait(); code != 0 {
				t.Fatalf("終了コード = %d, want 0 (ホストの goro run が、%v で落ちた・檻が死んだ)\nstdout:\n%s\nstderr:\n%s", code, sig, r.stdout.String(), r.stderr.String())
			}
			kv, _ := parseOut(r.stdout.String())
			want := map[syscall.Signal]string{syscall.SIGINT: "sigint=1 sigquit=0", syscall.SIGQUIT: "sigint=0 sigquit=1"}[sig]
			if !strings.Contains(r.stdout.String(), want) {
				t.Errorf("claude が受けたシグナル: want %q\nstdout:\n%s", want, r.stdout.String())
			}
			mask, err := strconv.ParseUint(kv["sigign"], 16, 64)
			if err != nil || mask&(1<<(uint(syscall.SIGINT)-1)|1<<(uint(syscall.SIGQUIT)-1)) != 0 {
				t.Errorf("claude の起動時に、SIGINT・SIGQUIT が無視になっている (SigIgn = %q): ホストの無視が、引き継がれた", kv["sigign"])
			}
			if !strings.Contains(r.stderr.String(), "セッション:") {
				t.Errorf("終了後の案内が出ていない:\n%s", r.stderr.String())
			}
		})
	}
}

// SIGTERM・SIGHUP は、ホストの goro run が受けて、檻を止め、後始末 (UDS の削除・案内) をして、128+番号で終わる。
func TestRunTermSignalsStopCage(t *testing.T) {
	f := newRunFixture(t)
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGTERM, 143}, {syscall.SIGHUP, 129}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			r := f.start(t, "run", "--repo", f.repo, "--", "hold")
			r.waitStdout("ready")
			if err := syscall.Kill(r.cmd.Process.Pid, tc.sig); err != nil { // ホストの goro run だけに送る
				t.Fatal(err)
			}
			if code := r.wait(); code != tc.code { // wait は、檻の中の全員が、標準出力を閉じるまで戻らない
				t.Errorf("終了コード = %d, want %d\nstderr:\n%s", code, tc.code, r.stderr.String())
			}
			serr := r.stderr.String()
			if !strings.Contains(serr, "檻を止めた") || !strings.Contains(serr, "セッション:") {
				t.Errorf("後始末の案内が出ていない:\n%s", serr)
			}
			id := sessionIDRE.FindStringSubmatch(serr)[1]
			if _, err := os.Lstat(filepath.Join(f.stateDir(), "sessions", id, "run", proxySockName)); err == nil {
				t.Error("終了後も、egress の UDS が残っている")
			}
			// ロックも離れている: 同じセッションを、もう一度使える。
			f.goro(t, "run", "--session", id, "--", "info").mustOK(t)
		})
	}
}

// 同じセッションを、同時に 2 つの goro run が使うことは、断る (UDS を取り合わない)。1 つ目は、影響を受けない。
func TestRunSameSessionTwiceRefused(t *testing.T) {
	f := newRunFixture(t)
	first := f.start(t, "run", "--repo", f.repo, "--", "hold")
	first.waitStdout("ready")
	ents, err := os.ReadDir(filepath.Join(f.stateDir(), "sessions"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("セッションが 1 つのはず: %v, %v", ents, err)
	}
	id := ents[0].Name()
	second := f.goro(t, "run", "--session", id, "--", "info")
	if second.code != 1 || !strings.Contains(second.stderr, "別の goro run が使っている") {
		t.Errorf("2 つ目の goro run: %s", second)
	}
	// 1 つ目の UDS は、無事 (取り替えられていない)。
	c, err := net.Dial("unix", filepath.Join(f.stateDir(), "sessions", id, "run", proxySockName))
	if err != nil {
		t.Errorf("1 つ目の egress の UDS に届かない: %v", err)
	} else {
		c.Close()
	}
	syscall.Kill(first.cmd.Process.Pid, syscall.SIGTERM)
	first.wait()
}

// goro run の終了コードは、claude の終了コード。終わった後の案内は、失敗しても出る。
func TestRunExitCode(t *testing.T) {
	f := newRunFixture(t)
	for _, code := range []int{0, 1, 7, 42} {
		r := f.goro(t, "run", "--repo", f.repo, "--", "exit", strconv.Itoa(code))
		if r.code != code {
			t.Errorf("claude が %d で終わったとき、goro run の終了コード = %d\n%s", code, r.code, r)
		}
		if !strings.Contains(r.stderr, "セッション:") {
			t.Errorf("claude が %d で終わったとき、案内が出ていない:\n%s", code, r.stderr)
		}
	}
}

// legacyTIOCSTIOff は、TIOCSTI が無効 (legacy_tiocsti = 0) か。端末に直結する檻は、そうでないと、起動しない (sandbox/bwrap の仕様)。
func legacyTIOCSTIOff() (bool, string) {
	b, err := os.ReadFile("/proc/sys/dev/tty/legacy_tiocsti")
	if err != nil {
		return false, err.Error()
	}
	return strings.TrimSpace(string(b)) == "0", strings.TrimSpace(string(b))
}

// 檻の中のプロセスが端末の設定 (raw・-echo・-isig) を変えても、goro run は、終了後 (正常終了でも、SIGTERM での取り消しでも)、
// 起動前の termios (全部) に戻す。端末 (pty) は、標準入出力と制御端末にする。
func TestRunRestoresTerminal(t *testing.T) {
	f := newRunFixture(t)
	if off, v := legacyTIOCSTIOff(); !off {
		t.Skipf("legacy_tiocsti = %q: TIOCSTI が有効 (か確かめられない) ので、端末に直結する檻は起動しない", v)
	}
	for _, tc := range []struct {
		name string
		args []string
		term bool // SIGTERM で止める
	}{{"正常終了", []string{"rawtty"}, false}, {"SIGTERM での取り消し", []string{"rawtty", "hold"}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := openPty(t)
			var out syncBuffer
			go io.Copy(&out, master) // master を読み続ける (読まないと、pty の buffer が詰まる)
			before := termOf(t, slave)

			ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, f.exe, append([]string{"run", "--repo", f.repo, "--"}, tc.args...)...)
			cmd.Args[0] = "goro"
			cmd.Env = f.env
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			deadline := time.Now().Add(runTimeout)
			for !strings.Contains(out.String(), "raw-set") {
				select {
				case err := <-done:
					t.Fatalf("raw-set が出る前に、goro が終わった: %v\n%s", err, out.String())
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("raw-set が出ない:\n%s", out.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if termOf(t, slave) == before {
				t.Fatal("前提: 檻の中の raw 化で、端末の設定が変わっていない (検査が空振りになる)")
			}
			if tc.term {
				if err := syscall.Kill(cmd.Process.Pid, syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			} else if _, err := master.Write([]byte("x")); err != nil { // 偽の claude を、正常に終わらせる
				t.Fatal(err)
			}
			select {
			case err := <-done:
				var ee *exec.ExitError
				if tc.term != (errors.As(err, &ee) && ee.ExitCode() == 143) || (!tc.term && err != nil) {
					t.Errorf("goro の終わり方: %v (want %s)", err, map[bool]string{true: "143", false: "0"}[tc.term])
				}
			case <-time.After(runTimeout):
				t.Fatalf("goro が終わらない:\n%s", out.String())
			}
			if after := termOf(t, slave); after != before {
				t.Errorf("goro の終了後、端末の設定が戻っていない:\n before %+v\n after  %+v\n出力:\n%s", before, after, out.String())
			}
		})
	}
}

// 標準入力が端末でないときは、何も保存も戻しもしない (警告も出さない)。
func TestRunNoTerminalNoWarning(t *testing.T) {
	f := newRunFixture(t)
	r := f.goro(t, "run", "--repo", f.repo, "--", "exit", "0").mustOK(t)
	if strings.Contains(r.stderr, "端末の設定") {
		t.Errorf("端末でないのに、端末の設定の警告が出ている:\n%s", r.stderr)
	}
}

// 檻を起動できなかった (bwrap の Spec の拒否: ~/.claude の下の claude) ときは、原因だけを出し、必ず失敗する再開や、
// 中身の無い取り出しを案内しない。--login も同じ。
func TestRunStartFailureShowsOnlyCause(t *testing.T) {
	f := newRunFixture(t)
	bad := filepath.Join(f.home, ".claude", "bin", "claude") // ~/.claude の下は、bwrap の拒否リストに当たる
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("\x7fELF fake binary\n"), 0o755); err != nil { // #! で始めない (スクリプトは、その前に断られる)
		t.Fatal(err)
	}
	for _, args := range [][]string{{"run", "--repo", f.repo, "--bin", bad}, {"run", "--login", "--bin", bad}} {
		r := f.goro(t, args...)
		if r.code != 1 || !strings.Contains(r.stderr, "檻を起動できない") {
			t.Errorf("%v: 終了コード・原因の表示:\n%s", args, r)
		}
		for _, hint := range []string{"--session", "goro export", "セッション:", "次は:", "監査ログ"} {
			if strings.Contains(r.stderr, hint) {
				t.Errorf("%v: 起動に失敗したのに、案内 %q が出ている:\n%s", args, hint, r.stderr)
			}
		}
	}
}

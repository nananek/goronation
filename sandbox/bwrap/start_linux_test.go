package bwrap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// このファイルの、檻を起動するテスト (needBwrap を呼ぶもの) は、bwrap の無い環境では skip し、
// GORO_REQUIRE_BWRAP=1 の CI leg では、skip でなく fail する。

// shortDir は、Unix ドメインソケットを置くための、短い path の一時ディレクトリ (sun_path は 107 バイトまで)。
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// TestCageEnvironment は、檻の環境変数が、Spec.Env に書いたものだけ (と bwrap が足す PWD) であることを確認する。
// ホストの環境変数に、資格情報らしいものがあっても、檻には渡らない。
func TestCageEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"SSH_AUTH_SOCK": "/tmp/fake-agent", "GH_TOKEN": "ghp_fake", "ANTHROPIC_API_KEY": "sk-ant-fake",
		"AWS_SECRET_ACCESS_KEY": "fake", "GORO_HOST_ONLY": "fake", "HOME": "/home/fake-host-home",
	} {
		t.Setenv(k, v)
	}
	fh := newFakeHome(t)
	s := probeSpec(t, fh.host(), t.TempDir(), t.TempDir())
	r := runProbe(t, s)

	want := []string{"HOME=/home/goro", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "PWD=/work"}
	got := slices.Clone(r.Env)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("檻の環境変数:\n got %q\nwant %q (Spec.Env と PWD だけ)", got, want)
	}
}

// TestCageHidesHostFiles は、ホストの HOME の中の資格情報の偽物 (~/.ssh・~/.claude・~/.gnupg・~/.aws・SSH_AUTH_SOCK の実体など) が、
// 檻から見えないことを確認する。檻の中の全ファイルを探しても、偽物の中身 (marker) は見つからない。
func TestCageHidesHostFiles(t *testing.T) {
	fh := newFakeHome(t)
	work := t.TempDir()
	stats := append([]string{fh.Home, fh.Sock, filepath.Dir(fh.Sock), "/root", "/etc/shadow", "/run/user", "/var/run/docker.sock"}, fh.Files...)
	args := []string{"-marker", fh.Marker}
	for _, p := range stats {
		args = append(args, "-stat", p)
	}
	r := runProbe(t, probeSpec(t, fh.host(), work, t.TempDir(), args...))
	for _, p := range stats {
		if r.Stats[p] == "" {
			t.Errorf("%s が檻の中から見える", p)
		}
	}
	if len(r.MarkerHits) != 0 {
		t.Errorf("偽の資格情報の中身が、檻の中のファイルにある: %q", r.MarkerHits)
	}
}

// TestCageNetwork は、檻がネットワークに届かないこと (--unshare-net) を確認する。
// ホストの loopback で待ち受ける listener にも、外部にも、届かない。檻の中の loopback は使える (対照)。
func TestCageNetwork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	dials := []string{ln.Addr().String(), "1.1.1.1:443", "8.8.8.8:53", "192.0.2.1:80", "[2606:4700:4700::1111]:443"}
	args := []string{}
	for _, d := range dials {
		args = append(args, "-dial", d)
	}
	fh := newFakeHome(t)
	r := runProbe(t, probeSpec(t, fh.host(), t.TempDir(), t.TempDir(), args...))
	for _, d := range dials {
		if e, ok := r.Dials[d]; !ok || e == "" {
			t.Errorf("檻から %s に接続できた (error = %q)", d, e)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("ホストの loopback の listener が、檻からの接続を %d 件受けた", n)
	}
	if !slices.Equal(r.Ifaces, []string{"lo"}) {
		t.Errorf("檻のネットワークインターフェース = %q, want lo だけ", r.Ifaces)
	}
	if r.Loopback != "" {
		t.Errorf("檻の中の loopback を使えない (対照): %s", r.Loopback)
	}
}

// TestCageFilesystem は、檻の中で、/work に書ける・/usr に書けない・/tmp は檻専用・/proc と /dev が使えることを確認する。
func TestCageFilesystem(t *testing.T) {
	fh := newFakeHome(t)
	work, home := t.TempDir(), t.TempDir()
	writes := []string{"/work/w.txt", "/home/goro/h.txt", "/tmp/t.txt", "/usr/u.txt", "/usr/bin/u.txt"}
	args := []string{}
	for _, w := range writes {
		args = append(args, "-write", w)
	}
	r := runProbe(t, probeSpec(t, fh.host(), work, home, args...))

	for _, ok := range []string{"/work/w.txt", "/home/goro/h.txt", "/tmp/t.txt"} {
		if e := r.Writes[ok]; e != "" {
			t.Errorf("%s に書けない: %s", ok, e)
		}
	}
	for _, ro := range []string{"/usr/u.txt", "/usr/bin/u.txt"} {
		if e := r.Writes[ro]; !strings.Contains(e, "read-only file system") {
			t.Errorf("%s に書けてしまう (error = %q, want read-only file system)", ro, e)
		}
	}
	// /work は、ホストの作業用 dir そのもの: 檻の中で作ったファイルは、ホストから見える (すぐ消えるので、内容は見ない)。
	// /proc と /dev: 檻専用の /proc (プロセスは、檻の中の数個だけ)。/dev は、最小の一式だけ。
	if !r.ProcSelf {
		t.Error("/proc/self/status を読めない (--proc /proc が要る)")
	}
	if r.ProcPIDs == 0 || r.ProcPIDs > 10 {
		t.Errorf("/proc の pid の数 = %d, want 1〜10 (檻専用の pid namespace)", r.ProcPIDs)
	}
	if r.PID > 10 {
		t.Errorf("檻の中の pid = %d, want 小さい数 (檻専用の pid namespace)", r.PID)
	}
	for d, e := range r.DevOpen {
		if e != "" {
			t.Errorf("%s を読めない: %s (--dev /dev が要る)", d, e)
		}
	}
	allowedDev := []string{"null", "zero", "full", "random", "urandom", "tty", "fd", "stdin", "stdout", "stderr", "pts", "ptmx", "shm", "console", "core"}
	for _, d := range r.Dev {
		if !slices.Contains(allowedDev, d) {
			t.Errorf("/dev に、想定外の項目 %q がある (ホストのデバイスが見えている)", d)
		}
	}
	if r.Cwd != "/work" {
		t.Errorf("作業ディレクトリ = %q, want /work", r.Cwd)
	}
	t.Logf("檻の中: uid=%d pid=%d CapEff=%s", r.UID, r.PID, r.CapEff)
}

// TestCageWorkIsTheHostDirectory は、/work が、ホストの作業用ディレクトリの rw bind であること (書いたものがホストに残る) を確認する。
func TestCageWorkIsTheHostDirectory(t *testing.T) {
	needBwrap(t)
	fh := newFakeHome(t)
	work := t.TempDir()
	s := probeSpec(t, fh.host(), work, t.TempDir())
	s.Cmd = []string{"/opt/probe/probe", probeArg, "-noop"}
	// 書いたファイルを残す: probe の -write は消すので、Cmd を書き込みの 1 回だけにはできない。
	// 代わりに、ホストで作った marker ファイルを、檻の中から読む。
	if err := os.WriteFile(filepath.Join(work, "from-host.txt"), []byte(fh.Marker), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Cmd = []string{"/opt/probe/probe", probeArg, "-marker", fh.Marker}
	r := runProbe(t, s)
	if !slices.Equal(r.MarkerHits, []string{"/work/from-host.txt"}) {
		t.Errorf("/work のファイルが、檻の中から見えない: %q", r.MarkerHits)
	}
}

// echoServer は、path で待ち受け、1 行を読んで "<tag>:<行>" を返す Unix ドメインソケットのサーバ (プロキシに見立てたもの)。
func echoServer(t *testing.T, path, tag string) net.Listener {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "%s:%s", tag, line)
			}()
		}
	}()
	return l
}

// TestCageReachesBoundDirectorySocket は、ホストの Unix ドメインソケットを置いたディレクトリを ro で bind すると、
// 檻の中から、そのソケットに接続できることを確認する (egress のプロキシに届く方法)。
// ソケットは、檻の起動後に作り直しても届く (ファイルそのものを bind すると、古い inode を掴んで届かなくなる)。
func TestCageReachesBoundDirectorySocket(t *testing.T) {
	needBwrap(t)
	run := shortDir(t)
	sock := filepath.Join(run, "p.sock")
	l1 := echoServer(t, sock, "one")

	fh := newFakeHome(t)
	s := probeSpec(t, fh.host(), t.TempDir(), t.TempDir(), "-uds-twice", "/run/goro/p.sock")
	s.Binds = append(s.Binds, Bind{Src: run, Dst: "/run/goro"})
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr strings.Builder
	s.Stdin, s.Stdout, s.Stderr = stdinR, stdoutW, &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait(); stdoutW.Close() }()

	sc := bufio.NewScanner(stdoutR)
	next := func(prefix string) string {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("%s の行を読めない: %v (stderr: %s)", prefix, sc.Err(), stderr.String())
		}
		line := sc.Text()
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("行 = %q, want %s で始まる", line, prefix)
		}
		return line
	}
	if got := next("first:"); got != "first:one:ping:<nil>" {
		t.Errorf("1 回目の応答 = %q, want first:one:ping:<nil>", got)
	}
	// プロキシを作り直す (別の inode)。ファイルを bind していたら、ここで届かなくなる。
	l1.Close()
	os.Remove(sock)
	l2 := echoServer(t, sock, "two")
	defer l2.Close()
	io.WriteString(stdinW, "\n")
	stdinW.Close() // *os.File 以外の Stdin は、EOF になるまで Wait が戻らない (os/exec の仕様)
	if got := next("second:"); got != "second:two:ping:<nil>" {
		t.Errorf("作り直した後の応答 = %q, want second:two:ping:<nil>", got)
	}
	if err := <-waitErr; err != nil {
		t.Errorf("檻の probe が失敗した: %v (stderr: %s)", err, stderr.String())
	}

	// ro で bind したので、檻の中から、ソケットのディレクトリに書けない。
	r := runProbe(t, func() Spec {
		s := probeSpec(t, fh.host(), t.TempDir(), t.TempDir(), "-write", "/run/goro/x")
		s.Binds = append(s.Binds, Bind{Src: run, Dst: "/run/goro"})
		return s
	}())
	if e := r.Writes["/run/goro/x"]; !strings.Contains(e, "read-only file system") {
		t.Errorf("ソケットのディレクトリに書けてしまう (error = %q)", e)
	}
}

// startParent は、檻を起動して待つ親のプロセスを起動し、bwrap の pid を返す。
func startParent(t *testing.T, marker string) (parent *os.Process, bwrapPid int) {
	t.Helper()
	needBwrap(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	p, err := os.StartProcess(exe, []string{exe, parentArg, marker}, &os.ProcAttr{Files: []*os.File{nil, pw, os.Stderr}})
	pw.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Kill(); p.Wait() })
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "started ") {
		t.Fatalf("親が檻を起動できない: %q %v", line, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "started ")))
	if err != nil {
		t.Fatal(err)
	}
	return p, pid
}

// processesWith は、コマンドラインに marker を含む、生きているプロセス (ゾンビを除く) の pid。
func processesWith(marker string) []int {
	var pids []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil && strings.Contains(string(b), marker) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// TestCageDiesWithParent は、檻を起動した親が SIGKILL で死ぬと、檻 (bwrap と、檻の中のプロセス) も死ぬことを確認する。
func TestCageDiesWithParent(t *testing.T) {
	marker := fmt.Sprintf("goro-bwrap-parent-%d-%d", os.Getpid(), time.Now().UnixNano())
	parent, _ := startParent(t, marker)

	deadline := time.Now().Add(5 * time.Second)
	for len(processesWith(marker)) < 3 && time.Now().Before(deadline) { // 親・bwrap・檻の中の probe
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(processesWith(marker)); n < 3 {
		t.Fatalf("檻が起動していない: marker を持つプロセス %d 個 (親・bwrap・probe の 3 個以上のはず)", n)
	}
	if err := parent.Kill(); err != nil {
		t.Fatal(err)
	}
	parent.Wait()
	deadline = time.Now().Add(10 * time.Second)
	for len(processesWith(marker)) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if left := processesWith(marker); len(left) > 0 {
		t.Errorf("親が死んで 10 秒たっても、檻のプロセスが残っている: %v", left)
	}
}

// TestBwrapProcessIsClean は、bwrap 自身が、空の環境変数と、/ の作業ディレクトリで起動されることを確認する
// (ホストの環境変数は、檻だけでなく、bwrap のプロセスにも渡さない)。
func TestBwrapProcessIsClean(t *testing.T) {
	t.Setenv("GORO_HOST_ONLY", "fake")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/fake-agent")
	marker := fmt.Sprintf("goro-bwrap-clean-%d-%d", os.Getpid(), time.Now().UnixNano())
	_, pid := startParent(t, marker)
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if errors.Is(err, os.ErrPermission) {
		t.Skipf("bwrap の /proc/%d/environ を読めない (dumpable でない): %v", pid, err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 0 {
		t.Errorf("bwrap の環境変数 = %q, want 空", env)
	}
	// 親 (テストバイナリ) の cwd はテストの cwd だが、bwrap の cwd は / にする。
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err != nil || cwd != "/" {
		t.Errorf("bwrap の cwd = %q (%v), want /", cwd, err)
	}
}

// TestStartResolvesSymlinks は、機密の path を指す symlink を、機密でない名前で bind しても、Start が拒否することを確認する
// (Argv は文字列だけを見るので通す。symlink を辿った実際の path の検証は、Start の役目)。bwrap は要らない。
func TestStartResolvesSymlinks(t *testing.T) {
	fh := newFakeHome(t)
	links := t.TempDir()
	link := func(name, target string) string {
		p := filepath.Join(links, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	homeLink := link("home", fh.Home)
	homeTree := filepath.Join(t.TempDir(), "home-via-link")
	if err := os.Symlink(fh.Home, homeTree); err != nil {
		t.Fatal(err)
	}
	// ~/.kube 自体が symlink で、実体は HOME の中の別の場所 (InHome を付けても、機密の実体は bind できない)。
	if err := os.MkdirAll(filepath.Join(fh.Home, ".private/kube"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".private/kube", filepath.Join(fh.Home, ".kube")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		host Host
		src  string
		need string // 前提として在るべき path (無ければ skip)
		want string
	}{
		{"~/.ssh への symlink", fh.host(), link("ssh", filepath.Join(fh.Home, ".ssh")), "", "機密"},
		{"~/.claude への symlink", fh.host(), link("claude", filepath.Join(fh.Home, ".claude")), "", "機密"},
		{"HOME 自体への symlink", fh.host(), homeLink, "", "機密"},
		{"HOME の下 (機密でない) への symlink。InHome なし", fh.host(), link("notes", filepath.Join(fh.Home, "notes.txt")), "", "InHome"},
		{"symlink の HOME の下の実体。InHome なし", Host{Home: homeTree}, filepath.Join(fh.Home, "notes.txt"), "", "InHome"},
		{"SSH_AUTH_SOCK の実体への symlink", fh.host(), link("agent", fh.Sock), "", "機密"},
		{"/etc/ssh への symlink", fh.host(), link("etcssh", "/etc/ssh"), "/etc/ssh", "機密"},
		{"/ への symlink", fh.host(), link("root", "/"), "", "機密"},
		{"~/.kube 自体が symlink。その先を直接指定", fh.host(), filepath.Join(fh.Home, ".private/kube"), "", "実体"},
		{"symlink の HOME を通して ~/.ssh", Host{Home: homeTree}, filepath.Join(fh.Home, ".ssh"), "", "機密"},
		{"Host.Secrets の側が symlink", Host{Home: fh.Home, Secrets: []string{link("sockalias", fh.Sock)}}, fh.Sock, "", "機密"},
		{"存在しない Src", fh.host(), filepath.Join(links, "none"), "", "解決できない"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.need != "" {
				if _, err := os.Stat(c.need); err != nil {
					t.Skipf("前提の path が無い: %v", err)
				}
			}
			s := Spec{Host: c.host, Binds: []Bind{{Src: c.src, Dst: "/x", InHome: strings.HasSuffix(c.src, ".private/kube")}}, Cmd: []string{"/bin/true"}}
			c2, err := Start(context.Background(), s)
			if err == nil {
				c2.Wait()
				t.Fatal("Start が error にならない")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("Start の error = %q, want %q を含む", err, c.want)
			}
		})
	}

	// Argv は文字列だけを見るので、機密を指す symlink を通す (Start が拒否する、役割の分担)。
	s := Spec{Host: fh.host(), Binds: []Bind{{Src: link("ssh2", filepath.Join(fh.Home, ".ssh")), Dst: "/x"}}, Cmd: []string{"/bin/true"}}
	if _, err := s.Argv(); err != nil {
		t.Errorf("Argv が、symlink の Src を拒否した (辿るのは Start の役目): %v", err)
	}
}

// TestResolvedSrcsAreRealPaths は、bwrap に渡す Src が、symlink を辿った実際の path であることを確認する
// (検証した path と、bind する path が食い違う余地 (検証の後の差し替え) を狭める)。
func TestResolvedSrcsAreRealPaths(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	real, _ = filepath.EvalSymlinks(real)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	fh := newFakeHome(t)
	s := Spec{Host: fh.host(), Binds: []Bind{{Src: alias, Dst: "/x"}, {Src: "/usr", Dst: "/usr"}}, Cmd: []string{"/bin/true"}}
	r, err := s.withResolvedSrcs()
	if err != nil {
		t.Fatal(err)
	}
	if r.Binds[0].Src != real || r.Binds[1].Src != "/usr" {
		t.Errorf("解決後の Src = %q, want [%q /usr]", []string{r.Binds[0].Src, r.Binds[1].Src}, real)
	}
	if s.Binds[0].Src != alias {
		t.Error("元の Spec の Src を書き換えた")
	}
}

// TestStartResolvesProtectedTrees は、拒否する path (protectedTrees) 自体が symlink のとき、その先も拒否することを確認する
// (例: この sandbox の /etc/ssh は、/etc/.ro…/ssh への symlink)。protectedTrees に、一時的に symlink を足して試す。
func TestStartResolvesProtectedTrees(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	alias := filepath.Join(dir, "alias")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	old := protectedTrees
	t.Cleanup(func() { protectedTrees = old })
	protectedTrees = append(slices.Clone(old), alias)

	fh := newFakeHome(t)
	s := Spec{Host: fh.host(), Binds: []Bind{{Src: real, Dst: "/x"}}, Cmd: []string{"/bin/true"}}
	if _, err := s.Argv(); err != nil {
		t.Fatalf("Argv (字面だけ) が、実体を拒否した: %v", err)
	}
	c, err := Start(context.Background(), s)
	if err == nil {
		c.Wait()
		t.Fatal("Start が、保護する path の実体を bind させた")
	}
	if !strings.Contains(err.Error(), "実体") {
		t.Errorf("error = %q, want 実体 (symlink の先) の理由", err)
	}
}

// TestStartRejectsInvalidSpec は、Start が、Argv と同じ検証を行い、起動しないことを確認する。
func TestStartRejectsInvalidSpec(t *testing.T) {
	s := cageSpec()
	s.Env = append(s.Env, EnvVar{"SSH_AUTH_SOCK", "/tmp/agent"})
	c, err := Start(context.Background(), s)
	if err == nil {
		c.Wait()
		t.Fatal("Start が error にならない")
	}
	if !strings.Contains(err.Error(), "資格情報らしい") {
		t.Errorf("error = %q", err)
	}
}

// openPtmx は、端末 (pty の master) の *os.File を返す。開けない環境では skip する。
func openPtmx(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("/dev/ptmx を開けない: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// openPty は、pty の master と slave を開く (端末の *os.File。/dev/pts/N は、端末エミュレータが渡す、実際の形)。
// 開けない環境では skip する。
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master = openPtmx(t)
	var unlock, n int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("pty を unlock できない: %v", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("pty の番号を取れない: %v", e)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pty の slave を開けない: %v", err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// mkdev は、Linux の dev_t (major・minor から作る)。
func mkdev(major, minor uint64) uint64 {
	return (major&0xfff)<<8 | minor&0xff | (minor&^0xff)<<12 | (major&^0xfff)<<32
}

func TestIsTTYDevice(t *testing.T) {
	for _, tc := range []struct {
		major, minor uint64
		want         bool
	}{
		{4, 0, true}, {4, 1, true}, {4, 64, true}, {5, 0, true}, {5, 1, true}, {5, 2, true}, {2, 0, true}, {3, 0, true},
		{136, 0, true}, {136, 300, true}, {140, 7, true}, {143, 255, true}, {166, 0, true}, {188, 0, true}, {204, 64, true}, {205, 0, true},
		// 端末でないもの (すぐ隣の番号を含む)。
		{1, 3, false}, {1, 5, false}, {1, 9, false}, {0, 0, false}, {6, 0, false}, {7, 0, false}, {8, 0, false}, {10, 1, false},
		{135, 0, false}, {144, 0, false}, {165, 0, false}, {167, 0, false}, {187, 0, false}, {189, 0, false},
		{4096 + 4, 0, false}, {4096 + 136, 1, false}, // 上位ビットを持つ major (下位 12 ビットだけを見ると、端末に見える)
		{203, 0, false}, {206, 0, false}, {1, 300, false},
	} {
		if got := isTTYDevice(mkdev(tc.major, tc.minor)); got != tc.want {
			t.Errorf("isTTYDevice(%d:%d) = %v, want %v", tc.major, tc.minor, got, tc.want)
		}
	}
}

// setTIOCSTI は、TIOCSTI の sysctl の代わりに、内容が v のファイル (v が空なら、無いファイル) を読ませる。
func setTIOCSTI(t *testing.T, v string) {
	t.Helper()
	old := tiocstiPath
	t.Cleanup(func() { tiocstiPath = old })
	tiocstiPath = filepath.Join(t.TempDir(), "legacy_tiocsti")
	if v != "" {
		if err := os.WriteFile(tiocstiPath, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	null, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	tmp, _ := os.Create(filepath.Join(t.TempDir(), "f"))
	defer tmp.Close()
	_, ptySlave := openPty(t)
	for name, tc := range map[string]struct {
		f    *os.File
		want bool
	}{"/dev/ptmx (端末)": {openPtmx(t), true}, "/dev/pts/N (端末)": {ptySlave, true}, "/dev/null": {null, false}, "pipe": {pr, false}, "通常ファイル": {tmp, false}, "nil": {nil, false}} {
		if got := isTerminal(tc.f); got != tc.want {
			t.Errorf("isTerminal(%s) = %v, want %v", name, got, tc.want)
		}
	}
	var buf strings.Builder
	if stdioIsTerminal(nil, &buf, (*os.File)(nil)) {
		t.Error("*os.File 以外・nil は、端末ではない")
	}
	if !stdioIsTerminal(nil, io.Discard, openPtmx(t)) {
		t.Error("標準入出力のどれかが端末なら、端末に直結している")
	}
}

func TestCheckTIOCSTI(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		wantErr       bool
	}{{"0", "0\n", false}, {"1 (有効)", "1\n", true}, {"2", "2\n", true}, {"空", "\n", true}, {"想定外の値", "off", true}} {
		setTIOCSTI(t, tc.content)
		if err := checkTIOCSTI(); (err != nil) != tc.wantErr {
			t.Errorf("%s: checkTIOCSTI = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
	setTIOCSTI(t, "") // ファイルが無い (古い kernel か、確かめられない): 確かめられないので error (fail-closed)
	if err := checkTIOCSTI(); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sysctl が無いのに error にならない・理由が分からない: %v", err)
	}
}

// TestStartChecksTIOCSTIOnTerminal は、NewSession でなく、端末に直結して起動するとき、TIOCSTI が有効なら、
// 起動せずに error にすることを確認する。bwrap は要らない (起動の前に断る)。
func TestStartChecksTIOCSTIOnTerminal(t *testing.T) {
	tty := openPtmx(t)
	_, slave := openPty(t)
	setTIOCSTI(t, "1\n")
	for name, mod := range map[string]func(*Spec){
		"pty の slave (/dev/pts/N)": func(s *Spec) { s.Stdin = slave },
		"標準入力が端末":                  func(s *Spec) { s.Stdin = tty },
		"標準出力が端末":                  func(s *Spec) { s.Stdout = tty },
		"標準エラーが端末":                 func(s *Spec) { s.Stderr = tty },
		"全部が端末":                    func(s *Spec) { s.Stdin, s.Stdout, s.Stderr = tty, tty, tty },
		"sysctl が無い":               func(s *Spec) { s.Stdin = tty; setTIOCSTI(t, "") },
	} {
		t.Run(name, func(t *testing.T) {
			s := cageSpec()
			mod(&s)
			c, err := Start(context.Background(), s)
			if err == nil {
				c.Wait()
				t.Fatal("Start が error にならない (TIOCSTI が有効なまま、端末に直結した)")
			}
			if !strings.Contains(err.Error(), "TIOCSTI") {
				t.Errorf("error = %q, want TIOCSTI の理由", err)
			}
		})
	}
}

// TestStartWithTerminal は、TIOCSTI が無効なら、端末に直結して起動できること・NewSession なら確認しないことを確認する。
// 「端末でなければ (標準入出力も制御端末も) 確認しない」は、制御端末を持たない子プロセスが要るので、
// TestStartWithoutTerminalDoesNotCheckTIOCSTI (ctty_linux_test.go) が確かめる。
func TestStartWithTerminal(t *testing.T) {
	needBwrap(t)
	tty := openPtmx(t)
	fh := newFakeHome(t)
	for name, tc := range map[string]struct {
		tiocsti    string
		newSession bool
	}{
		"TIOCSTI 無効 (0)":    {"0\n", false},
		"NewSession は確認しない": {"1\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			setTIOCSTI(t, tc.tiocsti)
			s := probeSpec(t, fh.host(), t.TempDir(), t.TempDir(), "-noop")
			s.NewSession = tc.newSession
			s.Stdin, s.Stdout, s.Stderr = tty, tty, tty
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := Start(ctx, s)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if err := c.Wait(); err != nil {
				t.Errorf("檻の probe が失敗した: %v", err)
			}
		})
	}
}

package conformance

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nananek/goronation/core/sandbox"
)

// check は、全バックエンドが満たす項目 (P0)。capCheck は、Cap つきの項目: run が返す値 (満たしたか) が、宣言と一致することを確かめる。
type check struct {
	id  string
	run func(c *C)
}

type capCheck struct {
	id  string
	cap sandbox.Cap
	run func(c *C) (satisfied bool, detail string)
}

// p0Checks は、P0 の項目 (契約の文言の順)。
var p0Checks = []check{
	{"hidden-host", checkHiddenHost},
	{"env-clean", checkEnvClean},
	{"network-none", checkNetworkNone},
	{"egress-only", checkEgressOnly},
	{"write-scope", checkWriteScope},
	{"ro-in-rw", checkROInRW},
	{"parent-death", checkParentDeath},
	{"exit-status", checkExitStatus},
	{"signal", checkSignal},
	{"inherited-fd", checkInheritedFD},
	{"terminal", checkTerminal},
	{"terminal-detached", checkTerminalDetached},
	{"rejects", checkRejects},
}

// capChecks は、Cap つきの項目。
var capChecks = []capCheck{
	{"path-remap", sandbox.CapPathRemap, capPathRemap},
	{"kills-descendants", sandbox.CapKillsDescendants, capKillsDescendants},
	{"private-loopback", sandbox.CapPrivateLoopback, capPrivateLoopback},
	{"private-pids", sandbox.CapPrivatePIDs, capPrivatePIDs},
}

// listNames は、probe の一覧の結果 ("ok:a,b") から、名前を取り出す。一覧できなければ ok が false。
func listNames(res string) (names []string, ok bool) {
	s, ok := strings.CutPrefix(res, "ok:")
	if !ok {
		return nil, false
	}
	if s == "" {
		return nil, true
	}
	return strings.Split(s, ","), true
}

// hostExists は、ホストに、path が在るか。
func hostExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// checkHiddenHost は、ホストの偽の資格情報 (HOME・認証用 socket・見せていない dir) が、檻から見えないことを確かめる。
// 祖先のディレクトリは、在ってよい (ホストと同じ path で見せる配置では、見せる path までの鎖)。ただし、その中に、見せていないものが見えてはいけない。
func checkHiddenHost(c *C) {
	fx := c.fx
	hidden := append([]string{fx.home, fx.sockDir, fx.sock, fx.other, filepath.Join(fx.other, "f.txt")}, fx.homeFiles...)
	args := []string{"-marker", fx.marker, "-list", fx.root}
	for _, p := range hidden {
		args = append(args, "-stat", p)
	}
	rep := c.mustReport(c.spec(args...))
	for _, p := range hidden {
		if rep.Stats[p] == "" {
			c.t.Errorf("ホストの %s が、檻の中から見える", p)
		}
	}
	if len(rep.MarkerHits) != 0 {
		c.t.Errorf("偽の資格情報の中身が、檻の中のファイルにある: %q", rep.MarkerHits)
	}
	if names, ok := listNames(rep.Lists[fx.root]); ok {
		for _, n := range names {
			if n != "granted" {
				c.t.Errorf("祖先 %s の中に、見せていない %q が見える", fx.root, n)
			}
		}
	}
}

// checkEnvClean は、檻の環境変数が、Spec.Env (と、バックエンドが宣言した ExtraEnv) だけであることを確かめる。
// ホストの環境変数に、資格情報らしいものがあっても、檻には渡らない。
func checkEnvClean(c *C) {
	for k, v := range map[string]string{
		"SSH_AUTH_SOCK": "/tmp/fake-agent", "GH_TOKEN": "ghp_fake", "ANTHROPIC_API_KEY": "sk-ant-fake",
		"AWS_SECRET_ACCESS_KEY": "fake", "GORO_HOST_ONLY": "fake", "HOME": "/home/fake-host-home",
	} {
		c.t.Setenv(k, v)
	}
	s := c.spec()
	rep := c.mustReport(s)
	want := map[string]string{}
	for _, e := range s.Env {
		want[e.Key] = e.Value
	}
	got := map[string]string{}
	for _, kv := range rep.Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
		if w, ok := want[k]; ok {
			if v != w {
				c.t.Errorf("環境変数 %s = %q, want %q", k, v, w)
			}
		} else if !slices.Contains(c.caps.ExtraEnv, k) {
			c.t.Errorf("檻の環境変数に、Spec に無い %s がある (宣言した ExtraEnv: %v)", k, c.caps.ExtraEnv)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			c.t.Errorf("Spec.Env の %s が、檻に渡っていない", k)
		}
	}
}

// checkNetworkNone は、外部と、ホストの loopback で待ち受ける listener に、檻から届かないことを確かめる。
func checkNetworkNone(c *C) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conn.Close()
		}
	}()
	dials := []string{ln.Addr().String(), "1.1.1.1:443", "8.8.8.8:53", "192.0.2.1:80", "[2606:4700:4700::1111]:443"}
	var args []string
	for _, d := range dials {
		args = append(args, "-dial", d)
	}
	rep := c.mustReport(c.spec(args...))
	for _, d := range dials {
		if e, ok := rep.Dials[d]; !ok || e == "" {
			c.t.Errorf("檻から %s に接続できた (error = %q)", d, e)
		}
	}
	if n := accepted.Load(); n != 0 {
		c.t.Errorf("ホストの loopback の listener が、檻からの接続を %d 件受けた", n)
	}
}

// checkEgressOnly は、Egress の UDS にだけ届くことを確かめる: 見せた UDS には届き、別の UDS には届かず、UDS を作り直しても届く。
func checkEgressOnly(c *C) {
	fx := c.fx
	sock := filepath.Join(fx.run, "p.sock")
	l1 := echoServer(c.t, sock, "one")
	defer l1.Close()
	otherSock := filepath.Join(fx.other, "q.sock")
	lo := echoServer(c.t, otherSock, "other")
	defer lo.Close()
	egress := c.guest(fx.run, "/run/gc") + "/p.sock"
	withEgress := func(args ...string) sandbox.Spec {
		s := c.spec(args...)
		s.Read = append(s.Read, c.mount(fx.run, "/run/gc"))
		s.Egress = egress
		return s
	}

	rep := c.mustReport(withEgress("-uds", egress, "-uds", otherSock))
	if got := rep.UDS[egress]; got != "one:ping" {
		c.t.Errorf("Egress の UDS の応答 = %q, want one:ping", got)
	}
	if got := rep.UDS[otherSock]; !strings.HasPrefix(got, "error:") {
		c.t.Errorf("Egress でない UDS に届いた: %q", got)
	}

	// UDS を、檻の起動後に作り直しても届く (ソケットのファイルでなく、ディレクトリを見せているため)。
	lv := c.live(withEgress("-udstwice", egress))
	if got := lv.next("first:"); got != "first:one:ping:<nil>" {
		c.t.Errorf("1 回目の応答 = %q, want first:one:ping:<nil>", got)
	}
	l1.Close()
	os.Remove(sock)
	l2 := echoServer(c.t, sock, "two")
	defer l2.Close()
	io.WriteString(lv.stdin, "\n")
	if got := lv.next("second:"); got != "second:two:ping:<nil>" {
		c.t.Errorf("作り直した後の応答 = %q, want second:two:ping:<nil>", got)
	}
	if err := lv.finish(); err != nil {
		c.t.Errorf("檻の probe が失敗した: %v (stderr: %s)", err, lv.errb.String())
	}
}

// checkWriteScope は、ホストに残る書き込みが、Write の下だけであることを確かめる。Read と、基盤と、見せていない path への書き込みは、
// 失敗するか、ホストに届かない。Scratch (PathRemap のバックエンド) には、書けて、ホストに残らない。
func checkWriteScope(c *C) {
	fx := c.fx
	work, ro := c.guest(fx.work, "/work"), c.guest(fx.ro, "/ro")
	args := []string{
		"-write", work + "/w.txt", "-write", ro + "/r.txt", "-write", ro + "/new.txt",
		"-write", filepath.Join(fx.other, "u.txt"), "-write", filepath.Join(fx.root, "x.txt"), "-write", "/usr/x",
	}
	if c.caps.PathRemap {
		args = append(args, "-write", "/scratch/s.txt")
	}
	s := c.spec(args...)
	s.Read = append(s.Read, c.mount(fx.ro, "/ro"))
	if c.caps.PathRemap {
		s.Scratch = []string{"/scratch"}
	}
	rep := c.mustReport(s)

	if e := rep.Writes[work+"/w.txt"]; e != "" {
		c.t.Errorf("Write の %s/w.txt に書けない: %s", work, e)
	} else if b, err := os.ReadFile(filepath.Join(fx.work, "w.txt")); err != nil || string(b) != "x" {
		c.t.Errorf("Write の下に書いたものが、ホストに残っていない: %q, %v", b, err)
	}
	if e := rep.Writes[ro+"/r.txt"]; e == "" {
		c.t.Errorf("Read の %s/r.txt に書けた", ro)
	}
	if b, _ := os.ReadFile(filepath.Join(fx.ro, "r.txt")); string(b) != "ro" {
		c.t.Errorf("Read の r.txt が、書き換えられた: %q", b)
	}
	if e := rep.Writes["/usr/x"]; e == "" {
		c.t.Error("基盤 (/usr) に書けた")
	}
	for _, p := range []string{filepath.Join(fx.ro, "new.txt"), filepath.Join(fx.other, "u.txt"), filepath.Join(fx.root, "x.txt"), "/usr/x"} {
		if hostExists(p) {
			c.t.Errorf("檻の中の書き込みが、ホストの %s に届いた", p)
		}
	}
	if c.caps.PathRemap {
		if e := rep.Writes["/scratch/s.txt"]; e != "" {
			c.t.Errorf("Scratch に書けない: %s", e)
		}
	}
}

// checkROInRW は、Write の中にある、Read の path (ro の実行ファイルなど) が、ro のままであることを確かめる。
// mount を、内側から先に作ると、外側の rw に隠れて、ro のはずのものが書ける。
func checkROInRW(c *C) {
	fx := c.fx
	agent := filepath.Join(fx.work, "tools", "agent")
	guestAgent := c.guest(fx.work, "/work") + "/tools/agent"
	s := c.spec("-write", guestAgent)
	s.Read = append(s.Read, c.mount(agent, "/work/tools/agent")) // Write の work の中の 1 ファイルを、ro で
	rep := c.mustReport(s)
	if e := rep.Writes[guestAgent]; e == "" {
		c.t.Errorf("Write の中の Read の %s に書けた", guestAgent)
	}
	if b, _ := os.ReadFile(agent); string(b) != "original" {
		c.t.Errorf("Write の中の Read のファイルが、書き換えられた: %q", b)
	}
}

// processesWith は、コマンドラインに marker を含む、生きているプロセス (ゾンビを除く) の pid。
func processesWith(marker string) []int {
	var pids []int
	if ents, err := os.ReadDir("/proc"); err == nil {
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
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && strings.Contains(line, marker) {
			if pid, err := strconv.Atoi(f[0]); err == nil && pid != os.Getpid() {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// waitFor は、cond が真になるのを、limit まで待つ。
func waitFor(limit time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// killAll は、pids を殺す (テストの後始末)。
func killAll(pids []int) {
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

// checkParentDeath は、檻を起動した親が SIGKILL で死ぬと、檻 (と、その中のプロセス) も死ぬことを確かめる。
func checkParentDeath(c *C) {
	marker := fmt.Sprintf("goro-conformance-parent-%d-%d", os.Getpid(), time.Now().UnixNano())
	cmd, out := c.reexec("parent", nil, roleMarker+"="+marker)
	// 檻の中の probe が動く (起動器の準備が終わっている) まで待つ。準備の前に親を殺すと、起動器が「親の死」を設定する前で、検査にならない。
	ready := make(chan bool, 1)
	go func() {
		r := bufio.NewReader(out)
		for {
			line, err := r.ReadString('\n')
			if strings.TrimSpace(line) == "ready" {
				ready <- true
				return
			}
			if err != nil {
				ready <- false
				return
			}
		}
	}()
	select {
	case ok := <-ready:
		if !ok {
			c.t.Fatal("親の役が、檻を起動できなかった (標準エラーを見る)")
		}
	case <-time.After(runTimeout):
		c.t.Fatal("親の役の檻の中の probe が、動かない")
	}
	if !waitFor(5*time.Second, func() bool { return len(processesWith(marker)) >= 1 }) {
		c.t.Fatal("檻の中の probe が見つからない")
	}
	t := c.t
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if !waitFor(10*time.Second, func() bool { return len(processesWith(marker)) == 0 }) {
		left := processesWith(marker)
		killAll(left)
		t.Errorf("親が死んで 10 秒たっても、檻のプロセスが残っている: %v", left)
	}
}

// exitCode は、Wait の結果を、終了コードにする (nil は 0)。契約の *ExitError でなければ、-1。
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *sandbox.ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

// checkExitStatus は、檻のコマンドの終了コードと、シグナルでの死 (128 + 番号) が、*ExitError で伝わることを確かめる。
func checkExitStatus(c *C) {
	for _, tc := range []struct {
		args []string
		want int
	}{{[]string{"-exit", "0"}, 0}, {[]string{"-exit", "7"}, 7}, {[]string{"-selfkill"}, 128 + int(syscall.SIGKILL)}} {
		cage, err := c.start(c.spec(tc.args...))
		if err != nil {
			c.t.Fatalf("Start: %v", err)
		}
		werr := c.wait(cage)
		if got := exitCode(werr); got != tc.want {
			c.t.Errorf("%v の終了 = %d (%v), want %d", tc.args, got, werr, tc.want)
		}
	}
}

// checkSignal は、Signal(SIGTERM) で、檻のコマンドが止まり、Wait が戻ることを確かめる。
func checkSignal(c *C) {
	lv := c.live(c.spec("-hold", "goro-conformance-signal"))
	lv.next("ready")
	if err := lv.cage.Signal(syscall.SIGTERM); err != nil {
		c.t.Fatalf("Signal: %v", err)
	}
	// SIGTERM で止まった檻の終了は、正常終了か、シグナルで死んだ (128 + 番号) のどちらか。*ExitError でない error や、別の終了コードは、契約違反。
	if err := lv.finish(); err != nil && exitCode(err) != 128+int(syscall.SIGTERM) {
		c.t.Errorf("SIGTERM で止めた檻の終了 = %v (終了コード %d), want nil か 128+SIGTERM の *ExitError", err, exitCode(err))
	}
}

// checkTerminalDetached は、制御端末を持つ呼び手が、Terminal = false で起動した檻が、その端末を持たない (/dev/tty を開けない) ことを確かめる。
// 端末を持たない檻は、呼び手の端末に、入力を注入できない (git の檻など、端末が要らない檻の既定)。
func checkTerminalDetached(c *C) {
	master, slave, err := openPTY()
	if err != nil {
		c.t.Fatalf("pty を用意できない: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	cmd, out := c.reexecAttr("ctty", []*os.File{slave}, cttyAttr()) // 子の fd 3 = slave。子の制御端末になる
	b, _ := io.ReadAll(out)
	_ = cmd.Wait()
	text := string(b)
	if strings.Contains(text, "START_ERR:") {
		c.t.Fatalf("Terminal = false の Spec を、起動できなかった: %s", text)
	}
	var rep report
	found := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &rep) == nil {
			found = true
		}
	}
	if !found {
		c.t.Fatalf("probe の報告が無い (檻が動かなかった):\n%s", text)
	}
	if rep.TTYOpen == "" {
		c.t.Errorf("Terminal = false の檻が、呼び手の制御端末 (/dev/tty) を開けた (TIOCSTI の結果: %q)", rep.TTYInject)
	}
}

// checkInheritedFD は、起動する側のプロセスが持つ、CLOEXEC でない fd が、檻に届かないことを確かめる。
func checkInheritedFD(c *C) {
	f, err := os.CreateTemp(c.fx.root, "secret")
	if err != nil {
		c.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(c.fx.marker + "\n"); err != nil {
		c.t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		c.t.Fatal(err)
	}
	files := make([]*os.File, fdLeakFd-3+1)
	files[fdLeakFd-3] = f
	cmd, out := c.reexec("fdleak", files)
	b, _ := io.ReadAll(out)
	_ = cmd.Wait()
	text := string(b)
	if strings.Contains(text, "START_ERR:") {
		c.t.Fatalf("呼び手の役が、檻を起動できなかった: %s", text)
	}
	if strings.Contains(text, c.fx.marker) {
		c.t.Errorf("起動する側の fd %d が、檻の中で読めた", fdLeakFd)
	}
	if !strings.Contains(text, `"FdRead"`) {
		c.t.Errorf("probe の報告が無い (檻が動かなかった):\n%s", text)
	}
}

// checkTerminal は、端末 (pty) に直結して起動するとき、檻からホストの端末に、入力を注入 (TIOCSTI) できないことを確かめる。
// 起動を断る (ErrTerminalUnsafe) のは、契約どおり。
func checkTerminal(c *C) {
	master, slave, err := openPTY()
	if err != nil {
		c.t.Fatalf("pty を用意できない: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	s := c.spec("-tiocsti")
	s.Terminal = true
	s.Stdin, s.Stdout, s.Stderr = slave, slave, slave
	cage, err := c.start(s)
	if err != nil {
		if errors.Is(err, sandbox.ErrTerminalUnsafe) {
			return
		}
		c.t.Fatalf("Start: %v", err)
	}
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(master)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "{") {
				lines <- line
				return
			}
		}
	}()
	werr := c.wait(cage)
	select {
	case line := <-lines:
		var rep report
		if err := json.Unmarshal([]byte(line), &rep); err != nil {
			c.t.Fatalf("probe の報告を読めない: %v: %s", err, line)
		}
		if rep.Tiocsti == "" {
			c.t.Error("檻から、端末に TIOCSTI で注入できた (ホストの端末に、入力を送り込める)")
		}
	case <-time.After(5 * time.Second):
		c.t.Fatalf("端末に、probe の報告が出ない (Wait: %v)", werr)
	}
}

// checkRejects は、契約に反する Spec を、Prepare と Start が断る (ErrRejected を包んだ error。起動しない) ことを確かめる。
// symlink を辿った実体が機密の Spec は、Prepare (文字列だけを見る) では見えないので、Start だけを確かめる。
func checkRejects(c *C) {
	fx := c.fx
	link := filepath.Join(fx.ro, "sshlink")
	if err := os.Symlink(filepath.Join(fx.home, ".ssh"), link); err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.b.Prepare(c.spec()); err != nil {
		c.t.Fatalf("正しい Spec を、Prepare が断った: %v", err)
	}
	type bad struct {
		name        string
		mutate      func(*sandbox.Spec)
		stringLevel bool // Prepare (文字列だけ) でも見える
	}
	addRead := func(m sandbox.Mount) func(*sandbox.Spec) {
		return func(s *sandbox.Spec) { s.Read = append(s.Read, m) }
	}
	inHome := func(m sandbox.Mount) sandbox.Mount { m.InHome = true; return m }
	for _, tc := range []bad{
		{"HOME 自体", addRead(c.mount(fx.home, "/x")), true},
		{"~/.ssh (InHome を付けても)", addRead(inHome(c.mount(filepath.Join(fx.home, ".ssh"), "/x"))), true},
		{"~/.claude.json (InHome を付けても)", addRead(inHome(c.mount(filepath.Join(fx.home, ".claude.json"), "/x"))), true},
		{"HOME の下の、InHome の無い path", addRead(c.mount(filepath.Join(fx.home, "notes.txt"), "/x")), true},
		{"認証用 socket", addRead(c.mount(fx.sock, "/x")), true},
		{"認証用 socket の親", addRead(c.mount(fx.sockDir, "/x")), true},
		{"システムの path を Write に", func(s *sandbox.Spec) { s.Write = append(s.Write, c.mount("/usr", "/x")) }, true},
		{"資格情報らしい環境変数 (GH_TOKEN)", func(s *sandbox.Spec) { s.Env = append(s.Env, sandbox.EnvVar{Key: "GH_TOKEN", Value: "x"}) }, true},
		{"環境変数の重複", func(s *sandbox.Spec) { s.Env = append(s.Env, s.Env[0]) }, true},
		{"相対の Exec", func(s *sandbox.Spec) { s.Exec = "probe" }, true},
		{"相対の HostPath", addRead(sandbox.Mount{HostPath: "rel/dir"}), true},
		{"NUL を含む引数", func(s *sandbox.Spec) { s.Args = append(s.Args, "a\x00b") }, true},
		{"機密を指す symlink", addRead(c.mount(link, "/x")), false},
	} {
		s := c.spec()
		tc.mutate(&s)
		if tc.stringLevel {
			if _, err := c.b.Prepare(s); !errors.Is(err, sandbox.ErrRejected) {
				c.t.Errorf("%s: Prepare = %v, want ErrRejected", tc.name, err)
			}
		}
		cage, err := c.b.Start(c.ctx(), s)
		if err == nil {
			_ = cage.Signal(os.Kill)
			c.t.Errorf("%s: Start が、契約に反する Spec を起動した", tc.name)
		} else if !errors.Is(err, sandbox.ErrRejected) {
			c.t.Errorf("%s: Start = %v, want ErrRejected", tc.name, err)
		}
	}
}

// capPathRemap は、GuestPath を HostPath と変えた Spec が動くか (満たす)、拒否されるか (満たさない) を返す。
func capPathRemap(c *C) (bool, string) {
	const guest = "/opt/remap/probe"
	var out lockedBuf
	s := sandbox.Spec{
		Exec: guest, Args: []string{probeArg, "-stat", guest}, System: true,
		Read:   []sandbox.Mount{{HostPath: c.fx.exe, GuestPath: guest}},
		Env:    []sandbox.EnvVar{{Key: "PATH", Value: "/usr/bin:/bin"}},
		Stdout: &out,
	}
	cage, err := c.start(s)
	if err != nil {
		if errors.Is(err, sandbox.ErrRejected) {
			return false, "GuestPath を、ErrRejected で断った"
		}
		c.t.Fatalf("Start: %v", err)
	}
	werr := c.wait(cage)
	var rep report
	_ = json.Unmarshal([]byte(out.String()), &rep)
	return werr == nil && rep.Stats[guest] == "", fmt.Sprintf("Wait: %v・stat %s: %q", werr, guest, rep.Stats[guest])
}

// capKillsDescendants は、檻のコマンドが終わったとき、setsid した子孫が、全員終わるかを返す。
func capKillsDescendants(c *C) (bool, string) {
	marker := fmt.Sprintf("goro-conformance-desc-%d-%d", os.Getpid(), time.Now().UnixNano())
	res := c.run(c.spec("-spawn", marker))
	if res.waitErr != nil {
		c.t.Fatalf("子孫を起こす probe が失敗した: %v\n%s%s", res.waitErr, res.stdout, res.stderr)
	}
	gone := waitFor(5*time.Second, func() bool { return len(processesWith(marker)) == 0 })
	left := processesWith(marker)
	killAll(left)
	return gone, fmt.Sprintf("檻のコマンドの終了後に、子孫が %d 個残った", len(left))
}

// capPrivateLoopback は、檻の loopback の待ち受けに、ホストから届かないかを返す。
func capPrivateLoopback(c *C) (bool, string) {
	lv := c.live(c.spec("-listen"))
	port, ok := portOf(lv.next("port "))
	if !ok {
		c.t.Fatal("待ち受けの port を読めない")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if conn != nil {
		conn.Close()
	}
	_ = lv.finish()
	return err != nil, fmt.Sprintf("ホストから、檻の待ち受け 127.0.0.1:%d に接続 = %v", port, err)
}

// capPrivatePIDs は、檻から、ホストのプロセス (このテスト) が見えないかを返す。
func capPrivatePIDs(c *C) (bool, string) {
	pid := os.Getpid()
	if pid < 100 {
		c.t.Skipf("テストの pid (%d) が小さく、檻の中の pid と区別できない", pid)
	}
	rep := c.mustReport(c.spec("-kill0", strconv.Itoa(pid)))
	return rep.Kill0 == "esrch", fmt.Sprintf("檻から、ホストの pid %d へのシグナル 0 = %q", pid, rep.Kill0)
}

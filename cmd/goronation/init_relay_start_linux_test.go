//go:build linux

package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 子の起動まわり (ADR 0020・0029・0030) の、ホストでの確認: runInit を、このプロセスの中で (標準入力・標準出力を socketpair に差し替えて) 動かす。
// 子は、偽の opencode (スクリプト。--version に答え、それ以外は relay-upstream のヘルパー)。landlock は使わない (bwrap の檻のテストが確かめる)。

func init() {
	helpers["relay-upstream"] = fakeRelayUpstream
}

// fakeOpencodeScript は、--version に答え、それ以外は、偽の opencode (relay-upstream) を起動するスクリプト。version が空なら、opencode v2.0.99。
func fakeOpencodeScript(t *testing.T, version string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if version == "" {
		version = "opencode v2.0.99"
	}
	path := filepath.Join(t.TempDir(), "opencode")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '%s'; exit 0; fi\nshift\nexec %s %s relay-upstream \"$@\"\n", version, exe, helperArg)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// initRun は、動いている init (このプロセスの中) と、ホスト側の control・状態の出力・記録。
type initRun struct {
	t      *testing.T
	rc     *relayClient
	status *bufio.Reader
	ctl    *net.UnixConn
	stderr *syncBuffer
	port   int
	done   chan int
}

// startInit は、goronation init --relay-control を、script を子にして動かす。extra は、init の引数 (子の前) に足す。childArgs は、子の引数 (ポートの後ろ)。
func startInit(t *testing.T, script string, extra []string, childArgs ...string) *initRun {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return startInitOnPort(t, script, port, extra, childArgs...)
}

func startInitOnPort(t *testing.T, script string, port int, extra []string, childArgs ...string) *initRun {
	t.Helper()
	pair, err := newStdioPair()
	if err != nil {
		t.Fatal(err)
	}
	// 子の標準エラー出力 (偽の opencode の記録) は、init の os.Stderr から来る。init 自身の stderr (runInit の引数) と同じ記録に集める。
	stderr := &syncBuffer{}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(stderr, errR)
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = pair.AgentIn, pair.AgentOut, errW
	t.Cleanup(func() {
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
		errW.Close()
		syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 1, 0) // runInit が、このプロセスを dumpable=0 にした
		pair.In.Close()
		pair.Out.Close()
	})
	args := []string{"--listen", "127.0.0.1:0", "--upstream", "/run/goro-test.sock", "--no-forward-tty", "--non-dumpable",
		"--relay-control", "--relay-port", strconv.Itoa(port), "--relay-token-env", "OPENCODE_PASSWORD"}
	args = append(args, extra...)
	args = append(args, "--", script, "serve", strconv.Itoa(port))
	args = append(args, childArgs...)
	r := &initRun{t: t, rc: newRelayClient(pair.In), status: bufio.NewReader(pair.Out), ctl: pair.In, stderr: stderr, port: port, done: make(chan int, 1)}
	go func() { r.done <- runInit(args, r.stderr) }()
	return r
}

// statusLine は、init が標準出力に出す 1 行 (ADR 0030) を読む。
func (r *initRun) statusLine() string {
	r.t.Helper()
	ch := make(chan string, 1)
	go func() {
		line, _ := r.status.ReadString('\n')
		ch <- strings.TrimRight(line, "\n")
	}()
	select {
	case l := <-ch:
		return l
	case <-time.After(30 * time.Second):
		r.t.Fatalf("init の状態の行が出ない\nstderr:\n%s", r.stderr.String())
		return ""
	}
}

func (r *initRun) exitCode() int {
	r.t.Helper()
	select {
	case c := <-r.done:
		return c
	case <-time.After(30 * time.Second):
		r.t.Fatalf("init が終わらない\nstderr:\n%s", r.stderr.String())
		return -1
	}
}

func (r *initRun) record(prefix string) string {
	for _, l := range strings.Split(r.stderr.String(), "\n") {
		if v, ok := strings.CutPrefix(l, prefix+" "); ok {
			return v
		}
	}
	return ""
}

// 通し: 起動の証明が済むと {"ready":true} が出て、要求が通る。トークンは、子の環境変数 (OPENCODE_PASSWORD) にだけ渡り (argv に無い)、上流への
// Authorization と一致する。親の環境の同名の変数・旧名 (OPENCODE_SERVER_PASSWORD) は、子に渡らない。
func TestInitRelayReadyAndToken(t *testing.T) {
	t.Setenv("OPENCODE_PASSWORD", "parent-value")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "legacy-value")
	r := startInit(t, fakeOpencodeScript(t, ""), []string{"--relay-version-prefix", "opencode v2.0."})
	if got := r.statusLine(); got != `{"ready":true}` {
		t.Fatalf("状態の行 = %q\nstderr:\n%s", got, r.stderr.String())
	}
	resp, err := r.rc.HTTPClient().Post("http://opencode.invalid/api/session", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d\n%s", resp.StatusCode, r.stderr.String())
	}
	tok := r.record("TOKEN-ENV")
	if len(tok) != 43 || tok == "parent-value" {
		t.Errorf("子の OPENCODE_PASSWORD = %q (init が作った 43 文字のトークンのはず)", tok)
	}
	if got := r.record("LEGACY-ENV"); got != `""` {
		t.Errorf("旧名 OPENCODE_SERVER_PASSWORD が子に渡った: %s", got)
	}
	if argv := r.record("ARGV"); strings.Contains(argv, tok) || strings.Contains(argv, "parent-value") {
		t.Errorf("トークンが子の argv に出た: %s", argv)
	}
	var auth string
	for _, l := range strings.Split(r.stderr.String(), "\n") {
		if q, ok := strings.CutPrefix(l, "REQ "); ok {
			s, _ := strconv.Unquote(q)
			if m := basicTokenRE.FindStringSubmatch(s); m != nil {
				auth = m[1]
			}
		}
	}
	if dec, _ := base64.StdEncoding.DecodeString(auth); string(dec) != "opencode:"+tok {
		t.Errorf("Authorization = %q, want opencode:<子の環境のトークン>", dec)
	}
	r.ctl.Close() // control を閉じる: 子が止まり、init が終わる
	r.exitCode()
}

// L0 (ADR 0029): 起動の証明が出るまで、要求を受け付けない。期限が来たら、子を止めて fail closed (標準出力に {"error":…}・非 0)。
func TestInitRelayNoProofFailsClosed(t *testing.T) {
	old := relayProofTimeout
	relayProofTimeout = 700 * time.Millisecond
	t.Cleanup(func() { relayProofTimeout = old })
	r := startInit(t, fakeOpencodeScript(t, ""), nil, "noproof")
	waitRelayCond(t, "偽の opencode の起動", func() bool { return strings.Contains(r.stderr.String(), "UPSTREAM-READY") })
	// 証明の前に送られた要求は、読まれない (上流に届かない)。
	hc := r.rc.HTTPClient()
	hc.Timeout = 400 * time.Millisecond
	hc.Get("http://opencode.invalid/api/info")
	line := r.statusLine()
	if !strings.HasPrefix(line, `{"error":"`) || !strings.Contains(line, "起動の証明") {
		t.Errorf("状態の行 = %q", line)
	}
	if code := r.exitCode(); code != exitInit {
		t.Errorf("終了コード = %d, want %d", code, exitInit)
	}
	if strings.Contains(r.stderr.String(), "REQ ") {
		t.Errorf("起動の証明の前の要求が、上流に届いた:\n%s", r.stderr.String())
	}
}

// 起動の証明が合わない (別のポート・JSON でない・長すぎる・子が先に終わる) と、fail closed。url 以外のキーがあっても、url が合えば通る。
func TestInitRelayProofMismatch(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"wrongurl", "一致しない"}, {"notjson", "一致しない"}, {"longline", "超える"}, {"exit3", "終わった"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := startInit(t, fakeOpencodeScript(t, ""), nil, tc.mode)
			line := r.statusLine()
			if !strings.HasPrefix(line, `{"error":"`) || !strings.Contains(line, tc.want) {
				t.Errorf("状態の行 = %q, want error に %q", line, tc.want)
			}
			if code := r.exitCode(); code != exitInit {
				t.Errorf("終了コード = %d, want %d", code, exitInit)
			}
		})
	}
	t.Run("extrakeys", func(t *testing.T) {
		r := startInit(t, fakeOpencodeScript(t, ""), nil, "extrakeys")
		if got := r.statusLine(); got != `{"ready":true}` {
			t.Errorf("状態の行 = %q", got)
		}
		r.ctl.Close()
		r.exitCode()
	})
}

// opencode より先に、別のプロセスがポートを bind していると、opencode は起動に失敗し、証明は出ない (ADR 0029 の実測 2 を模す)。
func TestInitRelayPortSquatted(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r := startInitOnPort(t, fakeOpencodeScript(t, ""), l.Addr().(*net.TCPAddr).Port, nil)
	if line := r.statusLine(); !strings.HasPrefix(line, `{"error":"`) {
		t.Errorf("状態の行 = %q", line)
	}
	if code := r.exitCode(); code != exitInit {
		t.Errorf("終了コード = %d, want %d", code, exitInit)
	}
}

// 持ち主が SO_REUSEPORT を付けていると (別のプロセスが同じポートを共有できる)、証明が合っていても、起動を拒否する。付けていなければ通る (上の通しのテスト)。
func TestInitRelayRefusesReusePort(t *testing.T) {
	r := startInit(t, fakeOpencodeScript(t, ""), nil, "reuseport")
	line := r.statusLine()
	if !strings.HasPrefix(line, `{"error":"`) || !strings.Contains(line, "SO_REUSEPORT") {
		t.Errorf("状態の行 = %q", line)
	}
	if code := r.exitCode(); code != exitInit {
		t.Errorf("終了コード = %d, want %d", code, exitInit)
	}
	// 拒否のあと、子は止められている (ポートが解放されている)。
	waitRelayCond(t, "子の終了 (ポートの解放)", func() bool {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(r.port))
		if err != nil {
			return false
		}
		l.Close()
		return true
	})
}

// 版の検査 (ADR 0018・0030): --version の出力が prefix で始まらなければ、子を起動せずに拒否する。
func TestInitRelayVersionMismatch(t *testing.T) {
	r := startInit(t, fakeOpencodeScript(t, "opencode v1.18.33"), []string{"--relay-version-prefix", "opencode v2.0."})
	line := r.statusLine()
	if !strings.HasPrefix(line, `{"error":"`) || !strings.Contains(line, "版の検査") {
		t.Errorf("状態の行 = %q", line)
	}
	if code := r.exitCode(); code != exitInit {
		t.Errorf("終了コード = %d, want %d", code, exitInit)
	}
	if strings.Contains(r.stderr.String(), "UPSTREAM-READY") {
		t.Error("版が違うのに、子が起動した")
	}
}

// pidfd (ADR 0029 の L1): 生きている子は「生きている」、終わった (回収前・回収後とも) 子は「生きていない」。
func TestPidfdAlive(t *testing.T) {
	var pidfd int
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{PidFD: &pidfd}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(pidfd)
	if !pidfdAlive(pidfd) {
		t.Fatal("生きている子が、生きていないことになった")
	}
	cmd.Process.Kill()
	waitRelayCond(t, "子の終了の検知", func() bool { return !pidfdAlive(pidfd) }) // 回収 (Wait) の前: ゾンビでも「終了」
	cmd.Wait()
	if pidfdAlive(pidfd) {
		t.Error("回収した後、生きていることになった")
	}
	if (&relayRun{pidfd: -1}).childAlive() {
		t.Error("pidfd が無いとき、生きていることになった (fail closed のはず)")
	}
}

func TestLandlockWrap(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	exe, _ := os.Executable()
	os.Args = []string{"/jail/goronation", "init", "--listen", "x", "--", "/opt/opencode/opencode"}
	name, args, err := landlockWrap("3128,8080", []string{"/opt/opencode/opencode", "serve"})
	want := []string{"landlock-exec", "--allow-connect", "3128,8080", "--", "/opt/opencode/opencode", "serve"}
	if err != nil || name != exe || strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("landlockWrap = %q %q %v", name, args, err)
	}
	// テストバイナリのように、サブコマンドの前に引数があれば、同じ形で再起動する。
	os.Args = []string{exe, helperArg, "goronation", "init", "--", "x"}
	_, args, _ = landlockWrap("1", []string{"/x"})
	if strings.Join(args[:3], " ") != helperArg+" goronation landlock-exec" {
		t.Errorf("前置きの引数が引き継がれない: %q", args)
	}
	os.Args = []string{exe, "serve"}
	if _, _, err := landlockWrap("1", []string{"/x"}); err == nil {
		t.Error("init が無い起動で、エラーにならない")
	}
}

func TestCheckChildVersion(t *testing.T) {
	sh := func(script string) error {
		return checkChildVersion("/bin/sh", []string{"-c", script}, nil, "opencode v2.0.")
	}
	if err := sh("echo opencode v2.0.20"); err != nil {
		t.Errorf("正しい版が断られた: %v", err)
	}
	for name, script := range map[string]string{
		"違う版":        "echo opencode v1.18.33",
		"先頭が違う (空白)": "echo ' opencode v2.0.20'",
		"出力なし":       "true",
		"失敗":         "echo opencode v2.0.20; exit 1",
		"stderr だけ":  "echo opencode v2.0.20 >&2",
	} {
		if err := sh(script); err == nil {
			t.Errorf("%s が通った", name)
		}
	}
	// 出力が大きくても、先頭だけ貯めて終わる (止まらない・先頭の検査はできる)。
	if err := sh("echo opencode v2.0.1; head -c 10000000 /dev/zero | tr '\\0' a"); err != nil {
		t.Errorf("大きな出力で失敗: %v", err)
	}
}

func TestWithoutEnv(t *testing.T) {
	got := withoutEnv([]string{"A=1", "OPENCODE_PASSWORD=x", "OPENCODE_SERVER_PASSWORD=y", "B=2=3", "OPENCODE_PASSWORD=z"}, "OPENCODE_PASSWORD", legacyTokenEnv)
	if strings.Join(got, ",") != "A=1,B=2=3" {
		t.Errorf("withoutEnv = %q", got)
	}
}

func TestInitArgsRelayFlags(t *testing.T) {
	base := []string{"--listen", "127.0.0.1:0", "--upstream", "/run/x.sock", "--non-dumpable", "--relay-control", "--relay-port", "4096"}
	parse := func(extra ...string) error {
		_, err := parseInitArgs(append(append([]string{}, base...), extra...), io.Discard)
		return err
	}
	for name, tc := range map[string]struct {
		extra []string
		ok    bool
	}{
		"トークンの環境変数名が無い":     {[]string{"--", "/c"}, false},
		"環境変数名が不正 (=)":      {[]string{"--relay-token-env", "A=B", "--", "/c"}, false},
		"環境変数名が不正 (数字で始まる)": {[]string{"--relay-token-env", "1A", "--", "/c"}, false},
		"正しい":                {[]string{"--relay-token-env", "OPENCODE_PASSWORD", "--", "/c"}, true},
		"landlock と絶対 path":  {[]string{"--relay-token-env", "T", "--landlock-connect", "3128", "--", "/c"}, true},
		"landlock だが相対 path": {[]string{"--relay-token-env", "T", "--landlock-connect", "3128", "--", "c"}, false},
		"landlock のポートが不正":   {[]string{"--relay-token-env", "T", "--landlock-connect", "0", "--", "/c"}, false},
	} {
		if err := parse(tc.extra...); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
	}
	for _, f := range [][]string{{"--relay-token-env", "T"}, {"--landlock-connect", "3128"}, {"--relay-version-prefix", "v"}} {
		args := append(append([]string{"--listen", "127.0.0.1:0", "--upstream", "/run/x.sock"}, f...), "--", "/c")
		if _, err := parseInitArgs(args, io.Discard); err == nil {
			t.Errorf("--relay-control なしで %v が通った", f)
		}
	}
}

// 横取りの窓の測定 (ADR 0029 の L1): 子 (偽の opencode) を SIGKILL で殺した直後に、同じ uid の別のプロセス (squatter) が同じポートを bind し、
// 待ち受ける。その間も、ホストが要求を送り続ける。squatter に、トークン (Authorization) が届いた試行の数を数える。
// 試行の数は GORO_SQUAT_TRIALS (未設定なら skip)。0 件が期待だが、0 でなければ、その率を ADR に書く (推測で 0 と言わない)。
func TestInitRelayKillAndSquatWindow(t *testing.T) {
	trials, _ := strconv.Atoi(os.Getenv("GORO_SQUAT_TRIALS"))
	if trials == 0 {
		t.Skip("GORO_SQUAT_TRIALS (試行の数) が無い")
	}
	leaked, reachedSquatter := 0, 0
	for i := 0; i < trials; i++ {
		r := startInit(t, fakeOpencodeScript(t, ""), nil)
		if got := r.statusLine(); got != `{"ready":true}` {
			t.Fatalf("試行 %d: 状態の行 = %q", i, got)
		}
		pid, _ := strconv.Atoi(r.record("PID"))
		var got atomicString
		stop := make(chan struct{})
		squatted := make(chan struct{})
		go func() { // squatter: ポートが空くまで bind を試し続け、取れたら、届いたものを記録する
			var l net.Listener
			for {
				var err error
				if l, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(r.port)); err == nil {
					break
				}
				select {
				case <-stop:
					return
				default:
				}
			}
			defer l.Close()
			close(squatted)
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
					b, _ := io.ReadAll(c)
					got.add(string(b))
				}()
			}
		}()
		var senders sync.WaitGroup
		for k := 0; k < 8; k++ { // 要求を送り続ける (子が死んだ後も、control が閉じるまで)
			senders.Add(1)
			go func() {
				defer senders.Done()
				hc := r.rc.HTTPClient()
				hc.Timeout = 500 * time.Millisecond
				for {
					select {
					case <-stop:
						return
					default:
					}
					resp, err := hc.Get("http://opencode.invalid/api/info")
					if err != nil && strings.Contains(err.Error(), "broken pipe") {
						return
					}
					if err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}
			}()
		}
		time.Sleep(20 * time.Millisecond)
		syscall.Kill(pid, syscall.SIGKILL)
		r.exitCode()
		time.Sleep(50 * time.Millisecond)
		close(stop)
		senders.Wait()
		select {
		case <-squatted:
			reachedSquatter++
		default:
		}
		if strings.Contains(got.String(), "Authorization: Basic") {
			leaked++
			t.Logf("試行 %d: squatter にトークンが届いた:\n%s", i, got.String())
		}
		r.ctl.Close()
	}
	t.Logf("試行 %d 回・squatter が bind できた %d 回・トークンが届いた %d 回", trials, reachedSquatter, leaked)
	if leaked != 0 {
		t.Errorf("トークンが squatter に届いた: %d / %d", leaked, trials)
	}
}

type atomicString struct {
	mu sync.Mutex
	s  string
}

func (a *atomicString) add(s string) { a.mu.Lock(); a.s += s; a.mu.Unlock() }
func (a *atomicString) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.s
}

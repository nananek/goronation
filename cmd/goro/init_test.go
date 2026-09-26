package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	helpers["sigrecorder"] = helperSigRecorder
	helpers["printenv"] = helperPrintEnv
	helpers["proxyclient"] = helperProxyClient
	helpers["waitstdin"] = helperWaitStdin
}

// helperSigRecorder は、転送されるシグナルを受けるたびに、その名前を 1 行で出す。SIGTERM を受けたら、5 で終わる。
func helperSigRecorder([]string) int {
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, forwardedSignals...)
	fmt.Println("ready")
	for s := range ch {
		fmt.Println(s.String())
		if s == syscall.SIGTERM {
			return 5
		}
	}
	return 1
}

// helperPrintEnv は、args の名前の環境変数のうち、設定されているものを NAME=VALUE で出す。
func helperPrintEnv(args []string) int {
	for _, name := range args {
		if v, ok := os.LookupEnv(name); ok {
			fmt.Printf("%s=%s\n", name, v)
		}
	}
	return 0
}

// helperProxyClient は、HTTPS_PROXY のアドレスへ繋いで "ping" を送り、EOF を伝えて、応答を出す。
func helperProxyClient([]string) int {
	proxy, ok := strings.CutPrefix(os.Getenv("HTTPS_PROXY"), "http://")
	if !ok {
		fmt.Fprintf(os.Stderr, "HTTPS_PROXY が http:// で始まらない: %q\n", os.Getenv("HTTPS_PROXY"))
		return 1
	}
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(testTimeout))
	if _, err := c.Write([]byte("ping")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%s", b)
	return 0
}

// helperWaitStdin は、"ready" を出し、stdin が EOF になるまで待って、3 で終わる。
func helperWaitStdin([]string) int {
	fmt.Println("ready")
	io.Copy(io.Discard, os.Stdin)
	return 3
}

// initCmd は、goro init (テストバイナリの再実行) を、child を子として起動するコマンドを返す。
// 標準入出力は呼び出し側が決める。testTimeout か、テストの終了で、子を含めて強制終了する。
func initCmd(t *testing.T, listen, upstream string, extra []string, child ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	self := selfCmd(t, "goro")
	args := append(self[1:], "init", "--listen", listen, "--upstream", upstream)
	args = append(args, extra...)
	args = append(args, "--")
	args = append(args, child...)
	cmd := exec.CommandContext(ctx, self[0], args...)
	// goro init だけを kill すると、子が stdout の pipe を握ったまま残る。プロセスグループごと kill する。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}

// exitCode は、cmd の終了コードを返す。シグナルで死んだ (ExitCode が -1) ときも、テストの失敗にする。
func exitCode(t *testing.T, err error, cmd *exec.Cmd) int {
	t.Helper()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("goro init を実行できない: %v", err)
	}
	code := cmd.ProcessState.ExitCode()
	if code < 0 {
		t.Fatalf("goro init がシグナルで死んだ: %v", cmd.ProcessState)
	}
	return code
}

func TestInitExitCode(t *testing.T) {
	notExec := filepath.Join(t.TempDir(), "notexec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		child []string
		want  int
	}{
		{"0", []string{"sh", "-c", "exit 0"}, 0},
		{"0 以外", []string{"sh", "-c", "exit 42"}, 42},
		{"SIGKILL", []string{"sh", "-c", "kill -KILL $$"}, 128 + 9},
		{"SIGTERM", []string{"sh", "-c", "kill -TERM $$"}, 128 + 15},
		{"見つからない", []string{"goro-test-no-such-command"}, exitNotFound},
		{"path が無い", []string{"/goro-test/no/such/file"}, exitNotFound},
		{"実行できない", []string{notExec}, exitNoExec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := initCmd(t, "127.0.0.1:0", "/nonexistent/up.sock", nil, tc.child...)
			err := cmd.Run()
			if got := exitCode(t, err, cmd); got != tc.want {
				t.Errorf("終了コード = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInitStdio(t *testing.T) {
	cmd := initCmd(t, "127.0.0.1:0", "/nonexistent/up.sock", nil, "sh", "-c", "cat; echo out; echo err >&2")
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader("in\n"), &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if got := stdout.String(); got != "in\nout\n" {
		t.Errorf("stdout = %q, want %q (stdin と stdout が子に繋がる)", got, "in\nout\n")
	}
	if got := stderr.String(); got != "err\n" {
		t.Errorf("stderr = %q, want %q (正常なら、init は何も書かない)", got, "err\n")
	}
}

// startSigRecorder は、goro init (extra の引数つき) の子として sigrecorder を起動し、"ready" が出るまで待つ。
// next は、子が出した次の 1 行を返す (5 秒来なければ、テストを止める)。
func startSigRecorder(t *testing.T, extra []string) (cmd *exec.Cmd, next func() string) {
	t.Helper()
	cmd = initCmd(t, "127.0.0.1:0", "/nonexistent/up.sock", extra, selfCmd(t, "sigrecorder")...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	next = func() string {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("子の出力が途切れた")
			}
			return line
		case <-time.After(5 * time.Second):
			t.Fatal("子の出力が 5 秒来ない (シグナルが転送されない)")
			return ""
		}
	}
	if got := next(); got != "ready" {
		t.Fatalf("最初の行 = %q, want ready", got)
	}
	return cmd, next
}

func TestInitForwardsSignals(t *testing.T) {
	cmd, next := startSigRecorder(t, nil)

	// 1 つずつ送り、子が受けたことを見てから、次を送る (init 自身は、これらで死なない)。
	for _, sig := range []syscall.Signal{syscall.SIGWINCH, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM} {
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		if got := next(); got != sig.String() {
			t.Fatalf("%v を送ったが、子が受けたのは %q", sig, got)
		}
	}

	// 子は SIGTERM で 5 で終わる。init は、その終了コードで終わる。
	err := cmd.Wait()
	if got := exitCode(t, err, cmd); got != 5 {
		t.Errorf("終了コード = %d, want 5", got)
	}
}

// --no-forward-tty: 端末のシグナル (SIGWINCH・SIGINT・SIGQUIT) は、子に転送しない (子が端末を共有して、直接受けるとき、
// 転送すると二重に届く)。init 自身は、受けても終わらない。SIGHUP・SIGTERM は、これまで通り転送する。
func TestInitNoForwardTTY(t *testing.T) {
	cmd, next := startSigRecorder(t, []string{"--no-forward-tty"})

	// 転送されないものを先に送る。転送されていれば、子の出力の最初の行が、これらになる。
	for _, sig := range []syscall.Signal{syscall.SIGWINCH, syscall.SIGINT, syscall.SIGQUIT} {
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		if got := next(); got != sig.String() {
			t.Fatalf("%v を送ったが、子が受けたのは %q (端末のシグナルが、転送されている)", sig, got)
		}
	}
	err := cmd.Wait()
	if got := exitCode(t, err, cmd); got != 5 {
		t.Errorf("終了コード = %d, want 5 (init が、端末のシグナルで終わった)", got)
	}
}

// proxyEnvNames は、init が設定するはずの 4 つと、設定しないはずの NO_PROXY。
var proxyEnvNames = []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy"}

// withEnv は、os.Environ() から names を除き、add を足した環境変数を返す。
func withEnv(names []string, add ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(names, key) {
			env = append(env, kv)
		}
	}
	return append(env, add...)
}

// printEnv は、goro init の子に printenv を実行させ、子から見えた NAME=VALUE の表を返す。
func printEnv(t *testing.T, env []string, extra []string, listen string) map[string]string {
	t.Helper()
	names := append([]string{"GORO_TEST_MARKER"}, proxyEnvNames...)
	cmd := initCmd(t, listen, "/nonexistent/up.sock", extra, selfCmd(t, "printenv", names...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return got
}

func TestInitProxyEnv(t *testing.T) {
	// 親の環境に、proxy の変数 (init が上書きするもの) と、無関係な変数がある。NO_PROXY は無い。
	parent := withEnv(proxyEnvNames, "HTTPS_PROXY=http://parent.example:1", "http_proxy=http://parent.example:2", "GORO_TEST_MARKER=kept")

	t.Run("空きポート", func(t *testing.T) {
		env := printEnv(t, parent, nil, "127.0.0.1:0")
		var port string
		for _, name := range proxyEnvNames[:4] {
			v, ok := strings.CutPrefix(env[name], "http://127.0.0.1:")
			if !ok {
				t.Fatalf("%s = %q, want http://127.0.0.1:PORT", name, env[name])
			}
			if port == "" {
				port = v
			} else if v != port {
				t.Errorf("%s のポート = %s, want %s (4 つとも同じ)", name, v, port)
			}
		}
		if n, err := strconv.Atoi(port); err != nil || n == 0 {
			t.Errorf("ポート = %q, want 実際に待ち受けているポート (0 ではない)", port)
		}
		if v, ok := env["NO_PROXY"]; ok {
			t.Errorf("NO_PROXY = %q, want 未設定", v)
		}
		if v, ok := env["no_proxy"]; ok {
			t.Errorf("no_proxy = %q, want 未設定", v)
		}
		if env["GORO_TEST_MARKER"] != "kept" {
			t.Errorf("無関係な環境変数が引き継がれない: %v", env)
		}
	})

	t.Run("指定したポート", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listen := l.Addr().String()
		l.Close()
		env := printEnv(t, parent, nil, listen)
		for _, name := range proxyEnvNames[:4] {
			if want := "http://" + listen; env[name] != want {
				t.Errorf("%s = %q, want %q", name, env[name], want)
			}
		}
	})

	t.Run("--no-proxy-env", func(t *testing.T) {
		env := printEnv(t, parent, []string{"--no-proxy-env"}, "127.0.0.1:0")
		want := map[string]string{
			"HTTPS_PROXY":      "http://parent.example:1",
			"http_proxy":       "http://parent.example:2",
			"GORO_TEST_MARKER": "kept",
		}
		if fmt.Sprint(env) != fmt.Sprint(want) {
			t.Errorf("環境変数 = %v, want %v (親のまま。init は触らない)", env, want)
		}
	})
}

// 子は、環境変数のアドレスに繋ぐだけで、init のリレー越しに、ホストの上流へ届く。
func TestInitRelaysThroughProxyEnv(t *testing.T) {
	sock := tempSock(t)
	seen := make(chan string, 1)
	startUpstream(t, sock, func(c net.Conn) {
		defer c.Close()
		b, _ := io.ReadAll(c)
		seen <- string(b)
		c.Write(append([]byte(hostSawPrefix), b...))
	})

	cmd := initCmd(t, "127.0.0.1:0", sock, nil, selfCmd(t, "proxyclient")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if got, want := stdout.String(), hostSawPrefix+"ping"; got != want {
		t.Errorf("子が受けた応答 = %q, want %q", got, want)
	}
	if got := <-seen; got != "ping" {
		t.Errorf("上流が受けたバイト列 = %q, want ping", got)
	}
}

// 子が終われば、接続が開いたままでも、待たずに終わり、リスナーを閉じる。
func TestInitExitsWithOpenConnection(t *testing.T) {
	sock := tempSock(t)
	startUpstream(t, sock, func(c net.Conn) {
		defer c.Close()
		io.Copy(io.Discard, c) // client が閉じるまで、開いておく
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := l.Addr().String()
	l.Close()

	cmd := initCmd(t, listen, sock, nil, selfCmd(t, "waitstdin")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatalf("最初の行 = %q, want ready", line)
	}

	// 子が動いている間は、リレーが受け付けている。この接続は、開いたままにする。
	c := dialTCP(t, listen)
	c.Write([]byte("open"))

	stdin.Close() // 子が 3 で終わる
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if got := exitCode(t, err, cmd); got != 3 {
			t.Errorf("終了コード = %d, want 3", got)
		}
	case <-time.After(testTimeout):
		t.Fatal("子が終わったのに、開いた接続を待って、init が終わらない")
	}
	if c2, err := net.DialTimeout("tcp", listen, time.Second); err == nil {
		c2.Close()
		t.Error("init の終了後も、リスナーが開いている")
	}
}

func TestParseInitArgs(t *testing.T) {
	const listen, upstream = "127.0.0.1:0", "/run/goro/egress.sock"
	base := []string{"--listen", listen, "--upstream", upstream}

	t.Run("受け付ける", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
			want initConfig
		}{
			{"-- あり", append(append([]string{}, base...), "--", "claude", "-p"),
				initConfig{listen, upstream, false, []string{"claude", "-p"}, false}},
			{"-- なし。CMD 以降は、flag に見えても子の引数", append(append([]string{}, base...), "claude", "--listen", "x"),
				initConfig{listen, upstream, false, []string{"claude", "--listen", "x"}, false}},
			{"-- の後の - で始まるコマンド", append(append([]string{}, base...), "--", "--odd"),
				initConfig{listen, upstream, false, []string{"--odd"}, false}},
			{"--no-proxy-env", append(append([]string{}, base...), "--no-proxy-env", "--", "c"),
				initConfig{listen, upstream, true, []string{"c"}, false}},
			{"--no-forward-tty", append(append([]string{}, base...), "--no-forward-tty", "--", "c"),
				initConfig{listen, upstream, false, []string{"c"}, true}},
			{"= で書く", []string{"--listen=127.0.0.1:8080", "--upstream=" + upstream, "c"},
				initConfig{"127.0.0.1:8080", upstream, false, []string{"c"}, false}},
			{"IPv6 の loopback", []string{"--listen", "[::1]:0", "--upstream", upstream, "c"},
				initConfig{"[::1]:0", upstream, false, []string{"c"}, false}},
			{"127.0.0.0/8 の loopback", []string{"--listen", "127.0.0.2:0", "--upstream", upstream, "c"},
				initConfig{"127.0.0.2:0", upstream, false, []string{"c"}, false}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var stderr bytes.Buffer
				got, err := parseInitArgs(tc.args, &stderr)
				if err != nil {
					t.Fatalf("%v: %s", err, stderr.String())
				}
				if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", tc.want) {
					t.Errorf("cfg = %+v, want %+v", got, tc.want)
				}
			})
		}
	})

	// 不正な引数は、使い方を stderr に出して 2 で終わり、子を起動しない。
	marker := filepath.Join(t.TempDir(), "started")
	touch := []string{"sh", "-c", "touch " + marker}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"引数なし", []string{}, "--listen が要る"},
		{"--listen なし", append([]string{"--upstream", upstream}, touch...), "--listen が要る"},
		{"--listen が空", append([]string{"--listen", "", "--upstream", upstream}, touch...), "--listen が要る"},
		{"--upstream なし", append([]string{"--listen", listen}, touch...), "--upstream は絶対 path"},
		{"--upstream が相対", append([]string{"--listen", listen, "--upstream", "up.sock"}, touch...), "--upstream は絶対 path"},
		{"CMD なし", base, "コマンドが要る"},
		{"-- の後に CMD なし", append(append([]string{}, base...), "--"), "コマンドが要る"},
		{"loopback ではない", append([]string{"--listen", "0.0.0.0:0", "--upstream", upstream}, touch...), "loopback"},
		{"IPv6 の全アドレス", append([]string{"--listen", "[::]:0", "--upstream", upstream}, touch...), "loopback"},
		{"ホスト名", append([]string{"--listen", "localhost:0", "--upstream", upstream}, touch...), "loopback"},
		{"ポートなし", append([]string{"--listen", "127.0.0.1", "--upstream", upstream}, touch...), "loopback"},
		{"ポートが範囲外", append([]string{"--listen", "127.0.0.1:65536", "--upstream", upstream}, touch...), "loopback"},
		{"未知の flag", append([]string{"--bogus", "--listen", listen, "--upstream", upstream}, touch...), "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := runInit(tc.args, &stderr); got != exitUsage {
				t.Errorf("終了コード = %d, want %d", got, exitUsage)
			}
			if s := stderr.String(); !strings.Contains(s, tc.want) || !strings.Contains(s, "使い方: goro init") {
				t.Errorf("stderr に %q と使い方が無い: %q", tc.want, s)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("引数が不正なのに、子が起動された")
			}
		})
	}

	t.Run("-h", func(t *testing.T) {
		var stderr bytes.Buffer
		if got := runInit([]string{"-h"}, &stderr); got != 0 {
			t.Errorf("終了コード = %d, want 0", got)
		}
		if !strings.Contains(stderr.String(), "使い方: goro init") {
			t.Errorf("使い方が出ない: %q", stderr.String())
		}
	})
}

// listen できないとき (使用中) は、子を起動せず、125 で終わる。
func TestInitListenFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	marker := filepath.Join(t.TempDir(), "started")

	var stderr bytes.Buffer
	got := runInit([]string{"--listen", l.Addr().String(), "--upstream", "/nonexistent/up.sock", "--", "sh", "-c", "touch " + marker}, &stderr)
	if got != exitInit {
		t.Errorf("終了コード = %d, want %d", got, exitInit)
	}
	if !strings.Contains(stderr.String(), "待ち受けられない") {
		t.Errorf("理由が出ない: %q", stderr.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("待ち受けられないのに、子が起動された")
	}
}

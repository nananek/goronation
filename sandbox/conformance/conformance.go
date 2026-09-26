package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nananek/goronation/core/sandbox"
	"github.com/nananek/goronation/sandbox/contract"
)

// NewBackend は、ホストの状態 host を持つバックエンドを作る関数。適合テストが、偽の HOME を作ってから、それを渡す。
type NewBackend func(host contract.Host) sandbox.Backend

// Option は、Run の設定。
type Option func(*options)

type options struct {
	requireEnv string
}

// RequireEnv は、バックエンドを使えないとき、環境変数 name が "1" なら skip でなく fail にする設定 (既定は GORO_REQUIRE_SANDBOX)。
func RequireEnv(name string) Option {
	return func(o *options) { o.requireEnv = name }
}

// runTimeout は、檻 1 つの起動から終了までの上限。
const runTimeout = 30 * time.Second

// C は、1 つの項目 (サブテスト) の文脈: テスト・バックエンド・宣言・fixture。
type C struct {
	t    *testing.T
	nb   NewBackend
	b    sandbox.Backend
	caps sandbox.Capabilities
	fx   *fixture
	top  string // Run を呼んだテストの名前 (役の再実行で、同じテストを指す)
}

// with は、サブテスト t の文脈を返す。
func (c *C) with(t *testing.T) *C {
	d := *c
	d.t = t
	return &d
}

// Run は、バックエンド (nb が作る) が契約を満たすかを、実際に檻を起動して確かめる。バックエンドを使えない (Probe が失敗する) ときは、
// RequireEnv の環境変数が 1 なら fail、そうでなければ skip する。
func Run(t *testing.T, nb NewBackend, opts ...Option) {
	t.Helper()
	o := options{requireEnv: "GORO_REQUIRE_SANDBOX"}
	for _, f := range opts {
		f(&o)
	}
	if role := os.Getenv(roleEnv); role != "" {
		runRole(t, nb, role)
		return
	}
	fx := newFixture(t)
	b := nb(fx.host)
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	if err := b.Probe(ctx); err != nil {
		if os.Getenv(o.requireEnv) == "1" {
			t.Fatalf("%s=1 だが、バックエンド %s を使えない: %v", o.requireEnv, b.Name(), err)
		}
		t.Skipf("バックエンド %s を使えないため skip する (%s=1 で必須になる): %v", b.Name(), o.requireEnv, err)
	}
	c := &C{t: t, nb: nb, b: b, caps: b.Capabilities(), fx: fx, top: t.Name()}
	t.Logf("バックエンド %s: %+v", b.Name(), c.caps)
	for _, ch := range p0Checks {
		t.Run(ch.id, func(t *testing.T) { ch.run(c.with(t)) })
	}
	for _, ch := range capChecks {
		t.Run("cap-"+ch.id, func(t *testing.T) {
			d := c.with(t)
			got, detail := ch.run(d)
			if want := d.caps.Has(ch.cap); got != want {
				t.Errorf("Capabilities.%s の宣言 = %v だが、実際は %v (%s)", ch.cap, want, got, detail)
			}
		})
	}
}

// guest は、論理的な配置の path logical (PathRemap のバックエンド) か、ホストの path host (それ以外) を返す。
func (c *C) guest(host, logical string) string {
	if c.caps.PathRemap {
		return logical
	}
	return host
}

// mount は、host を、guest (論理的な配置) で見せる Mount。PathRemap でなければ、GuestPath は付けない (ホストと同じ)。
func (c *C) mount(host, logical string) sandbox.Mount {
	m := sandbox.Mount{HostPath: host}
	if c.caps.PathRemap {
		m.GuestPath = logical
	}
	return m
}

// probeExe は、檻の中の probe の path。
func (c *C) probeExe() string { return c.guest(c.fx.exe, "/opt/probe/probe") }

// minimalSpec は、probe を、args で動かす最小の Spec: 基盤・probe の実行ファイル (Read)・許可リストの環境変数だけ。
func minimalSpec(caps sandbox.Capabilities, exeHost string, args ...string) sandbox.Spec {
	m := sandbox.Mount{HostPath: exeHost}
	exe := exeHost
	if caps.PathRemap {
		m.GuestPath, exe = "/opt/probe/probe", "/opt/probe/probe"
	}
	return sandbox.Spec{
		Exec: exe, Args: append([]string{probeArg}, args...), System: true, Read: []sandbox.Mount{m},
		Env: []sandbox.EnvVar{{Key: "PATH", Value: "/usr/bin:/bin"}, {Key: "LANG", Value: "C.UTF-8"}},
	}
}

// spec は、probe を args で動かす Spec: minimalSpec に、作業ディレクトリ (Write) と、HOME・cwd を足したもの。
func (c *C) spec(args ...string) sandbox.Spec {
	s := minimalSpec(c.caps, c.fx.exe, args...)
	work := c.guest(c.fx.work, "/work")
	s.Write = []sandbox.Mount{c.mount(c.fx.work, "/work")}
	s.Dir = work
	s.Env = append(s.Env, sandbox.EnvVar{Key: "HOME", Value: work})
	return s
}

// lockedBuf は、複数の goroutine から書ける bytes.Buffer。
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// result は、檻を最後まで動かした結果。
type result struct {
	rep            report
	hasReport      bool
	waitErr        error
	stdout, stderr string
}

// ctx は、テストの終わりに取り消す context (檻の起動に使う。取り消すと、起動器が殺される)。
func (c *C) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*runTimeout)
	c.t.Cleanup(cancel)
	return ctx
}

// start は、s で檻を起動する。起動できなければ、error を返す。檻は、テストの終わりに殺す。
func (c *C) start(s sandbox.Spec) (sandbox.Cage, error) {
	cage, err := c.b.Start(c.ctx(), s)
	if err != nil {
		return nil, err
	}
	c.t.Cleanup(func() { _ = cage.Signal(os.Kill) })
	return cage, nil
}

// wait は、檻の終了を、runTimeout まで待つ。終わらなければ、殺して、テストを止める。
func (c *C) wait(cage sandbox.Cage) error {
	c.t.Helper()
	done := make(chan error, 1)
	go func() { done <- cage.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(runTimeout):
		_ = cage.Signal(os.Kill)
		c.t.Fatalf("檻が %s で終わらない", runTimeout)
		return nil
	}
}

// run は、s で檻を起動し、終わるまで待って、probe の報告を読む。起動できなければ、テストを止める。
func (c *C) run(s sandbox.Spec) result {
	c.t.Helper()
	var out, errb lockedBuf
	s.Stdout, s.Stderr = &out, &errb
	cage, err := c.start(s)
	if err != nil {
		c.t.Fatalf("Start: %v", err)
	}
	res := result{waitErr: c.wait(cage), stdout: out.String(), stderr: errb.String()}
	res.hasReport = json.Unmarshal([]byte(res.stdout), &res.rep) == nil
	return res
}

// mustReport は、run して、正常終了と、probe の報告があることを確かめる。
func (c *C) mustReport(s sandbox.Spec) report {
	c.t.Helper()
	res := c.run(s)
	if res.waitErr != nil || !res.hasReport {
		c.t.Fatalf("probe が報告を返さなかった: %v\nstdout: %s\nstderr: %s", res.waitErr, res.stdout, res.stderr)
	}
	return res.rep
}

// live は、標準入出力を pipe でつないだ、動いている檻。
type live struct {
	c     *C
	cage  sandbox.Cage
	stdin *io.PipeWriter
	lines chan string
	done  chan error
	errb  *lockedBuf
}

// live は、s で檻を起動する (標準入力・標準出力は、pipe)。
func (c *C) live(s sandbox.Spec) *live {
	c.t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	l := &live{c: c, stdin: inW, lines: make(chan string, 64), done: make(chan error, 1), errb: &lockedBuf{}}
	s.Stdin, s.Stdout, s.Stderr = inR, outW, l.errb
	cage, err := c.start(s)
	if err != nil {
		c.t.Fatalf("Start: %v", err)
	}
	l.cage = cage
	go func() {
		err := cage.Wait()
		outW.Close()
		l.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			l.lines <- sc.Text()
		}
		close(l.lines)
	}()
	c.t.Cleanup(func() { inW.Close() })
	return l
}

// next は、prefix で始まる行が出るのを待つ。
func (l *live) next(prefix string) string {
	l.c.t.Helper()
	deadline := time.After(runTimeout)
	for {
		select {
		case line, ok := <-l.lines:
			if !ok {
				l.c.t.Fatalf("%q で始まる行が出る前に、檻の出力が閉じた (stderr: %s)", prefix, l.errb.String())
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
		case <-deadline:
			l.c.t.Fatalf("%q で始まる行が出ない (stderr: %s)", prefix, l.errb.String())
		}
	}
}

// finish は、標準入力を閉じて、檻の終了を待つ。
func (l *live) finish() error {
	l.c.t.Helper()
	l.stdin.Close()
	select {
	case err := <-l.done:
		return err
	case <-time.After(runTimeout):
		_ = l.cage.Signal(os.Kill)
		l.c.t.Fatalf("檻が %s で終わらない", runTimeout)
		return nil
	}
}

// 役 (テストバイナリの再実行) の環境変数。
const (
	roleEnv    = "GORO_CONFORMANCE_ROLE" // "parent": 檻を起動して待つ親 / "fdleak": fd を持ったまま檻を起動する呼び手
	roleExeEnv = "GORO_CONFORMANCE_EXE"  // 役が使う、probe の実行ファイル (ホストの path)
	roleMarker = "GORO_CONFORMANCE_MARKER"
)

// runRole は、再実行された役として動く (テストの本体は動かさない)。
func runRole(t *testing.T, nb NewBackend, role string) {
	t.Helper()
	b := nb(contract.Host{Home: "/nonexistent-home"})
	caps := b.Capabilities()
	exe, marker := os.Getenv(roleExeEnv), os.Getenv(roleMarker)
	switch role {
	case "parent":
		// probe の "ready" は、この役の標準出力にそのまま出る (檻が、起動器の準備を終えて、probe が動いたことの合図)。
		s := minimalSpec(caps, exe, "-hold", marker)
		s.Stdout, s.Stderr = os.Stdout, os.Stderr
		if _, err := b.Start(context.Background(), s); err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		select {}
	case "fdleak":
		s := minimalSpec(caps, exe, "-fdread", fmt.Sprint(fdLeakFd))
		s.Stdout, s.Stderr = os.Stdout, os.Stderr
		cage, err := b.Start(context.Background(), s)
		if err != nil {
			fmt.Println("START_ERR:", err)
			return
		}
		fmt.Println("CAGE_EXIT:", cage.Wait())
	default:
		t.Fatalf("未知の役 %q", role)
	}
}

// fdLeakFd は、fdleak の役が、CLOEXEC なしで持つ fd の番号。
const fdLeakFd = 9

// reexec は、テストバイナリを、役 role で再実行する (Run を呼んだテストだけを動かす)。標準出力は pipe で読む。
func (c *C) reexec(role string, files []*os.File, env ...string) (*exec.Cmd, io.ReadCloser) {
	c.t.Helper()
	self, err := os.Executable()
	if err != nil {
		c.t.Fatal(err)
	}
	var pat []string
	for _, part := range strings.Split(c.top, "/") {
		pat = append(pat, "^"+regexp.QuoteMeta(part)+"$")
	}
	cmd := exec.Command(self, "-test.run="+strings.Join(pat, "/"), "-test.count=1")
	cmd.Env = append(os.Environ(), append([]string{roleEnv + "=" + role, roleExeEnv + "=" + c.fx.exe}, env...)...)
	cmd.ExtraFiles = files
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		c.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, out
}

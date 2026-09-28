//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nananek/goronation/cmd/framecapture/connectproxy"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// harnessRunInput は、runHarness に渡す、エージェント (opencode・claude) ごとに異なる設定。
type harnessRunInput struct {
	// AgentName は、"opencode" か "claude" (PATH 探索・檻の中のファイル名に使う)。
	AgentName string
	// AgentBinFlag は、--agent-bin の値 (空なら PATH から探す)。
	AgentBinFlag string
	// GoroBinFlag は、--goronation-bin の値 (空なら PATH から探す)。
	GoroBinFlag string
	// FakeHandler は、tools/fakeproviders の Server.NewHTTPServer() が返す *http.Server
	// (ReadHeaderTimeout・ReadTimeout 設定済み)。
	FakeHandler *http.Server
	// FakeVirtualHost・FakeVirtualPort は、エージェントに見せる仮想のホスト名・ポート
	// (connectproxy の許可表の key。実際には解決しない)。
	FakeVirtualHost string
	FakeVirtualPort int
	// Env は、エージェント固有の環境変数。
	Env []bwrap.EnvVar
	// Args は、檻の中でエージェントに渡す引数。
	Args []string
	// Stdin は、檻のコマンドの標準入力 (nil なら空)。
	Stdin io.Reader
	// Out は、採取したフレームを書くファイルの path (空なら、runHarness の stdout にそのまま流す)。
	Out string
	// WorkSetup は、work ディレクトリへの下ごしらえ (opencode.json を書くなど)。nil なら何もしない。
	WorkSetup func(work string) error
}

// runHarness は、in の設定で、fake サーバー・connectproxy・檻を起こし、エージェントを 1 回動かして、
// 終了コードを返す。
func runHarness(in harnessRunInput, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "framecapture %s: "+format+"\n", append([]any{in.AgentName}, a...)...)
		return 1
	}

	goroExe, err := findBin(in.GoroBinFlag, "goronation")
	if err != nil {
		return fail("%v", err)
	}
	agentExe, err := findBin(in.AgentBinFlag, in.AgentName)
	if err != nil {
		return fail("%v", err)
	}

	stateDir, err := os.MkdirTemp("", "framecapture-")
	if err != nil {
		return fail("作業ディレクトリを作れない: %v", err)
	}
	defer os.RemoveAll(stateDir)

	home := filepath.Join(stateDir, "home")
	work := filepath.Join(stateDir, "work")
	runDir := filepath.Join(stateDir, "run")
	for _, d := range []string{home, work, runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fail("%s を作れない: %v", d, err)
		}
	}
	if in.WorkSetup != nil {
		if err := in.WorkSetup(work); err != nil {
			return fail("work の下ごしらえに失敗: %v", err)
		}
	}

	cert, err := newSelfSignedCert(in.FakeVirtualHost)
	if err != nil {
		return fail("自己署名証明書を作れない: %v", err)
	}
	caPath := filepath.Join(stateDir, "ca.pem")
	if err := os.WriteFile(caPath, cert.CAPEM, 0o600); err != nil {
		return fail("CA 証明書を書けない: %v", err)
	}

	fake, err := startFakeTLS(cert, in.FakeHandler)
	if err != nil {
		return fail("fake サーバーを起動できない: %v", err)
	}
	defer fake.Close()

	sockPath := filepath.Join(runDir, proxySockName)
	pl, err := net.Listen("unix", sockPath)
	if err != nil {
		return fail("connectproxy の UDS で待ち受けられない: %v", err)
	}
	target := fmt.Sprintf("%s:%d", in.FakeVirtualHost, in.FakeVirtualPort)
	real := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(fake.Port))
	proxy := connectproxy.New(map[string]netip.AddrPort{target: real})
	proxy.Logf = func(format string, args ...any) {
		fmt.Fprintf(stderr, "framecapture %s: connectproxy: "+format+"\n", append([]any{in.AgentName}, args...)...)
	}
	proxyErr := make(chan error, 1)
	go func() { proxyErr <- proxy.Serve(pl) }()

	var out io.Writer = stdout
	if in.Out != "" {
		f, err := os.Create(in.Out)
		if err != nil {
			proxy.Close()
			return fail("--out %s を開けない: %v", in.Out, err)
		}
		defer f.Close()
		out = f
	}

	host := bwrap.CurrentHost()
	cfg := cageConfig{
		Host: host, AgentExe: agentExe, AgentBinName: filepath.Base(agentExe), GoroExe: goroExe,
		CACertPEM: caPath, RunDir: runDir, Home: home, Work: work,
		Env: in.Env, Args: in.Args, Stdout: out, Stderr: stderr,
	}
	spec := cageSpec(cfg)
	spec.Stdin = in.Stdin

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	c, err := bwrap.Start(ctx, spec)
	if err != nil {
		proxy.Close()
		return fail("檻を起動できない: %v", err)
	}
	waitErr := c.Wait()

	proxy.Close() // Serve のループを終わらせる (idempotent)
	if err := <-proxyErr; err != nil {
		fmt.Fprintf(stderr, "framecapture %s: connectproxy が異常終了した: %v\n", in.AgentName, err)
	}
	return exitCodeOf(waitErr, stderr)
}

// exitCodeOf は、檻の Wait の結果を、終了コードにする (シグナルで死んだら 128+番号。
// cmd/goronation/run.go の exitCodeOf と同じ規則)。
func exitCodeOf(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		fmt.Fprintf(stderr, "framecapture: 檻を待てない: %v\n", err)
		return 1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

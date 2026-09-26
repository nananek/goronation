package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// goro init の終了コード。子の終了コードは、そのまま返す (シグナルなら 128+番号)。
const (
	exitUsage    = 2   // 引数が不正
	exitInit     = 125 // 子を起動する前に、init 自身が失敗した
	exitNoExec   = 126 // 子を実行できない
	exitNotFound = 127 // 子が見つからない
)

const initUsage = `使い方: goro init --listen 127.0.0.1:PORT --upstream PATH [--no-proxy-env] [--no-forward-tty] [--] CMD [ARGS...]

檻の中で最初に動く小さなリレー (goro run が起動する)。檻の loopback の TCP を、ホストの egress の Unix ドメインソケットへ
中継しながら、子 (CMD) を起動する。子には、標準入出力と環境変数を引き継ぎ、HTTPS_PROXY・HTTP_PROXY (小文字も) を、待ち受け先を
指す http の URL にして渡し、loopback (127.0.0.1・localhost・::1) は proxy を通さない (NO_PROXY・no_proxy。既存の値には足す)。SIGINT・SIGTERM・SIGHUP・SIGQUIT・SIGWINCH は子に転送し、自分が PID 1 の
ときは孤児を回収する。子の終了コード (シグナルで死んだら 128+番号) で終わる。引数の不正は 2、子を起動する前の失敗は 125、
実行できないは 126、見つからないは 127。リレーは、宛先を見ずにバイト列を通す (許可先の判定は、上流の egress が行う)。
init は、檻を作らず、自分が檻の中にいることも確かめない。

  --listen ADDR     檻の loopback で待ち受ける TCP (IP リテラル:ポート。ポート 0 は空きポート)
  --upstream PATH   中継先の Unix ドメインソケットの絶対 path
  --no-proxy-env    子に HTTPS_PROXY などを設定しない
  --no-forward-tty  端末のシグナル (SIGINT・SIGQUIT・SIGWINCH) を、子に転送しない (子が端末を共有し、直接受けるとき。二重に届かない)
`

// proxyEnvKeys は、init が子に設定する、proxy を指す環境変数。
var proxyEnvKeys = []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"}

// noProxyKeys・noProxyLoopback は、init が子に設定する、proxy を通さない宛先の環境変数と、その宛先 (檻の loopback)。
// 檻の中のプロセスが、同じ檻の中の別のプロセス (opencode の background service など) へ、http://127.0.0.1:PORT で繋ぐとき、
// proxy 変数のままだと、その通信が egress に届き、拒否される (reason=method・405)。loopback は、檻の中の名前空間の loopback で、
// ホストのものではない: 迂回しても、檻の外へは届かない (ネットワークが無い)。
var (
	noProxyKeys     = []string{"NO_PROXY", "no_proxy"}
	noProxyLoopback = []string{"127.0.0.1", "localhost", "::1"}
)

// forwardedSignals は、子に転送するシグナル。
var forwardedSignals = []os.Signal{
	syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH,
}

// ttySignals は、端末が、フォアグラウンドの process group 全体に送るシグナル。子が端末を共有していれば、子は、これらを直接受ける。
var ttySignals = []os.Signal{syscall.SIGINT, syscall.SIGQUIT, syscall.SIGWINCH}

type initConfig struct {
	listen     string
	upstream   string
	noProxyEnv bool
	argv       []string // 子のコマンドと引数
	noFwdTTY   bool     // 端末のシグナル (ttySignals) を、子に転送しない
}

// parseInitArgs は goro init の引数を解釈する。不正なら、理由と使い方を stderr に出して error を返す。
// -h のときは、使い方を出して flag.ErrHelp を返す。
func parseInitArgs(args []string, stderr io.Writer) (initConfig, error) {
	var cfg initConfig
	flags := flag.NewFlagSet("goro init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, initUsage) }
	flags.StringVar(&cfg.listen, "listen", "", "")
	flags.StringVar(&cfg.upstream, "upstream", "", "")
	flags.BoolVar(&cfg.noProxyEnv, "no-proxy-env", false, "")
	flags.BoolVar(&cfg.noFwdTTY, "no-forward-tty", false, "")
	if err := flags.Parse(args); err != nil {
		return cfg, err // flag が、理由と使い方を出している
	}
	cfg.argv = flags.Args()

	fail := func(format string, a ...any) (initConfig, error) {
		fmt.Fprintf(stderr, "goro init: "+format+"\n", a...)
		fmt.Fprint(stderr, initUsage)
		return cfg, errors.New("引数が不正")
	}
	if cfg.listen == "" {
		return fail("--listen が要る")
	}
	// 名前の解決 (localhost など) を許さず、loopback の IP リテラルだけを受け付ける。
	if ap, err := netip.ParseAddrPort(cfg.listen); err != nil || !ap.Addr().IsLoopback() {
		return fail("--listen は loopback の IP リテラル:ポートだけ (127.0.0.1:0 など): %q", cfg.listen)
	}
	if !filepath.IsAbs(cfg.upstream) {
		return fail("--upstream は絶対 path が要る: %q", cfg.upstream)
	}
	if len(cfg.argv) == 0 {
		return fail("起動するコマンドが要る")
	}
	return cfg, nil
}

// runInit は goro init の本体で、終了コードを返す。
func runInit(args []string, stderr io.Writer) int {
	cfg, err := parseInitArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}

	l, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		fmt.Fprintf(stderr, "goro init: 待ち受けられない: %v\n", err)
		return exitInit
	}
	defer l.Close()
	go newRelay(cfg.upstream, maxConns).serve(l)

	cmd := exec.Command(cfg.argv[0], cfg.argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = childEnv(os.Environ(), l.Addr().String(), !cfg.noProxyEnv)

	// 子を起動する前に登録する。起動の途中で届いたシグナルも、取りこぼさない。
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, forwardedSignals...)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "goro init: 子を起動できない: %v\n", err)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return exitNotFound
		}
		return exitNoExec
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				// 転送しない端末のシグナルも、ここで受ける (受けないと、init 自身が、既定の動作で終わる)。
				if cfg.noFwdTTY && slices.Contains(ttySignals, s) {
					continue
				}
				cmd.Process.Signal(s) // 子が終わっていれば失敗するが、構わない
			case <-done:
				return
			}
		}
	}()

	code := waitChild(cmd.Process.Pid, stderr)
	l.Close()
	return code
}

// mergeNoProxy は、env の NO_PROXY・no_proxy (同じ名前が複数あれば、最後のもの) の値を、この順に、重複なくまとめ、そこに noProxyLoopback の
// うち無いものを足した、コンマ区切りの値を返す (大文字小文字は区別せずに比べる)。
func mergeNoProxy(env []string) string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, v) }) {
			out = append(out, v)
		}
	}
	for _, key := range noProxyKeys {
		last := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				last = v
			}
		}
		for _, v := range strings.Split(last, ",") {
			add(v)
		}
	}
	for _, v := range noProxyLoopback {
		add(v)
	}
	return strings.Join(out, ",")
}

// childEnv は、setProxy なら、env の後ろに proxy 用の変数 (proxyAddr を指す) と、loopback を迂回する NO_PROXY・no_proxy を足して返す。
// 親の環境に同じ名前があっても、os/exec は、重複した名前の最後の値を使う。
func childEnv(env []string, proxyAddr string, setProxy bool) []string {
	if !setProxy {
		return env
	}
	out := slices.Clone(env)
	for _, key := range proxyEnvKeys {
		out = append(out, key+"=http://"+proxyAddr)
	}
	noProxy := mergeNoProxy(env)
	for _, key := range noProxyKeys {
		out = append(out, key+"="+noProxy)
	}
	return out
}

// waitChild は、pid の子が終わるまで待ち、その終了コードを返す (シグナルで死んだら 128+番号)。
// 自分が PID 1 のときは、孤児になったプロセスも回収する (wait4(-1))。
// cmd.Wait は使わない。PID 1 の wait4(-1) が子の終了を先に回収すると、cmd.Wait は終了コードを失うため。
func waitChild(pid int, stderr io.Writer) int {
	target := pid
	if os.Getpid() == 1 {
		target = -1
	}
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(target, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "goro init: wait4: %v\n", err)
			return exitInit
		}
		if got != pid { // 回収した孤児
			continue
		}
		switch {
		case ws.Exited():
			return ws.ExitStatus()
		case ws.Signaled():
			return 128 + int(ws.Signal())
		}
	}
}

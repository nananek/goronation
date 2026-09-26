package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// 檻の中では、テストバイナリが、実際の goro (/opt/goro/goro) と、偽の claude (/opt/claude/claude)・偽の opencode
// (/opt/opencode/opencode。同じ偽のエージェント) の代わりに動く:
// bwrap は、起動するコマンドを、その path を argv[0] にして実行するので、名前で選ぶ。init は、テストバイナリの
// TestMain より先に走る。syscall.Exit は、-race のバイナリの、終了時の 1 秒の待ち (atexit_sleep_ms) を避ける
// (檻に環境変数は渡らないので、GORACE では避けられない)。
func init() {
	switch filepath.Base(os.Args[0]) {
	case "goro":
		// 実プロセスの goro (子プロセス) にだけ、テスト用の第 3 の profile を足す (テストの本体のプロセスの agents は、本物の 2 つのまま)。
		// エージェントを足す作業は、この 1 つの profile を表に足すことだけ (TestRunThirdAgent)。
		agents = append(agents, testAgentProfile)
		syscall.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
	case "claude", "opencode", testAgentProfile.name:
		syscall.Exit(fakeClaude(os.Args[1:]))
	}
}

// screenStdout・screenStderr は、偽のエージェント (screen) が出す、TUI の画面に見えるバイト列。goro の解釈 (パターンマッチ・
// UTF-8 の検査・エスケープの除去など) が入ると、そのまま届かなくなる。
const (
	screenStdout = "\x1b[2J\x1b[H┌  Select integration\n│  Security notes: Login successful\n\xff\xfe\x00 not-utf8\n\x1b]0;title\x07Done\n"
	screenStderr = "■ Timed out waiting for the background service to start\n\x1b[31m└  Failed\x1b[0m\n"
)

// testAgentProfile は、テスト用の第 3 のエージェントの profile: 宛先・環境変数・認証情報の共有 (環境変数と symlink の両方)・種の定数を持つだけの、profile 1 つ。
// 偽のエージェントの実行ファイル (テストバイナリ) は、檻の中の path の名前 (/opt/fakeagent/fakeagent) で、偽のエージェントとして動く。
var testAgentProfile = agentProfile{
	name:       "fakeagent",
	exeExample: "/opt/fakeagent/bin/fakeagent",
	env:        []bwrap.EnvVar{{Key: "FAKEAGENT_MODE", Value: "test"}},
	creds: credentials{
		env:     []bwrap.EnvVar{{Key: "FAKEAGENT_AUTH_DIR", Value: jailAuth}},
		linkDir: ".local/share/fakeagent", files: []string{"auth.json", "mcp-auth.json"},
	},
	seed:        []homeFile{{path: ".config/fakeagent/seed.txt", content: "SEED-OF-FAKEAGENT\n"}},
	hosts:       func() []string { return []string{"fake.example:443"} },
	loginArgs:   []string{"login"},
	loginUsage:  "login を起動する (テスト用の説明。この文が -h に出る)",
	exitHint:    "/quit",
	resumeUsage: "fakeagent の続きの説明 (テスト用。-h だけに出る)",
}

// fakeClaude は、偽のエージェント (claude・opencode・テスト用の第 3 のエージェント)。最初の引数が、場面の名前で、結果を、標準出力に "キー=値" か "操作 => 結果" の行で出す
// (goro run は、標準入出力を、檻の中の claude に直結する)。場面は次の通り。
//
//	auth login [場面...]  opencode の --login のときの起動 (auth login)。HOME にログインの目印を作り、続きに場面があれば、それを動かす
//	                      (なければ、引数と環境を出す)
//	info                  引数・作業ディレクトリ・HOME・環境変数の名前・/work の中身を出す
//	probe OP...           OP (stat:PATH・write:PATH・dial:ADDR・mnt:PATH) を試して、結果を出す。mnt は、PATH の mount が ro か rw か
//	connect TARGET...     HTTPS_PROXY へ、TARGET の CONNECT を送り、応答の状態コードを出す
//	screen                エージェントの画面に見える出力 (エスケープ・項目名・エラー文・不正な UTF-8・NUL) を、標準出力と標準エラーに出し、
//	                      終了コード 3 で終わる (goro が、出力を読まず・解釈せず、そのまま通すことの確認)
//	loopback [ADDR]       proxy の環境変数を守るクライアント (Bun・Node と同じ: NO_PROXY の宛先は直接、それ以外は HTTP_PROXY 経由) で、ADDR
//	                      (無ければ、檻の中の loopback に自分で立てたサーバー) に GET し、"loopback => <状態コード> direct|proxy" を出す
//	commit FILE TEXT MSG  /work に FILE を書いて、git commit する
//	gitlog FILE           /work のコミットの一覧と、FILE の中身を出す
//	marker                認証用ディレクトリ (/auth) のログインの目印を読む
//	hwrite PATH TEXT...   HOME (/home/goro) の PATH に TEXT を書く (親のディレクトリも作る。PATH TEXT の組は、いくつでも)。"hwrite:PATH => ok" か "err: 理由"
//	hread PATH...         HOME の PATH を読み、"hread:PATH => <引用した中身>" (無ければ "err: 理由") を出す
//	hls                   HOME の下の、通常のファイルと symlink (symlink は "path->先") を、相対 path で並べて出す ("hls => a,b,c")
//	awrite NAME TEXT...   認証用ディレクトリ (/auth) の NAME に TEXT を、その場で書く (組は、いくつでも)。"awrite:NAME => ok" か "err: 理由"
//	arename NAME TEXT     認証用ディレクトリの NAME を、一時ファイルに書いて rename で置き換える (claude の認証情報の書き方。inode が変わる)
//	aread NAME...         認証用ディレクトリの NAME を読み、"aread:NAME => <引用した中身>" (無ければ "err: 理由") を出す
//	als                   認証用ディレクトリの下のファイルの名前を並べて出す ("als => a,b")
//	env NAME...           環境変数 NAME の値を、"env:NAME => 値" で出す
//	await NAME OLD        ready を出し、認証用ディレクトリの NAME の中身が OLD 以外になるのを待ち、"changed=<引用した中身>" を出す (実行中の檻への伝わり)
//	sigcount              ready を出し、最初のシグナルから 1 秒間の SIGINT・SIGQUIT の数を出す
//	hold                  ready を出し、殺されるまで待つ
//	exit N                終了コード N で終わる
//	rawtty [hold]         標準入力の端末を raw・-echo・-isig にして、raw-set を出す。hold なら、殺されるまで待つ。そうでなければ、
//	                      端末から 1 バイト届くまで待って終わる (raw の間に、テストが端末の設定を確かめられるように)
func fakeClaude(args []string) int {
	if len(args) == 0 {
		fmt.Println("scenario=none")
		return 0
	}
	switch args[0] {
	case "auth", "login":
		if err := os.WriteFile(filepath.Join(jailAuth, "login-marker"), []byte("logged-in\n"), 0o600); err != nil { // ログイン状態 = 認証情報は、認証用ディレクトリに残る
			fmt.Println("marker-write-error=" + err.Error())
		}
		if args[0] == "auth" && len(args) > 2 && args[1] == "login" && !strings.HasPrefix(args[2], "-") { // auth login <場面> ...: 続きの場面も動かす
			return fakeClaude(args[2:])
		}
		if args[0] == "login" && len(args) > 1 && !strings.HasPrefix(args[1], "-") { // login <場面> ... (第 3 のエージェント)
			return fakeClaude(args[1:])
		}
		return fakeInfo(args)
	case "info":
		return fakeInfo(args)
	case "probe":
		for _, op := range args[1:] {
			fmt.Printf("%s => %s\n", op, fakeProbe(op))
		}
		return 0
	case "connect":
		for _, target := range args[1:] {
			fmt.Printf("%s => %s\n", target, fakeConnect(target))
		}
		return 0
	case "screen":
		os.Stdout.WriteString(screenStdout)
		os.Stderr.WriteString(screenStderr)
		return 3
	case "loopback":
		return fakeLoopback(args[1:])
	case "commit":
		if len(args) != 4 {
			fmt.Println("commit: 引数が足りない")
			return 2
		}
		return fakeCommit(args[1], args[2], args[3])
	case "gitlog":
		out, err := exec.Command("/usr/bin/git", "-C", "/work", "log", "--format=log=%H").CombinedOutput()
		fmt.Print(string(out))
		if err != nil {
			fmt.Println("gitlog-error=" + err.Error())
			return 1
		}
		if len(args) > 1 {
			b, err := os.ReadFile(filepath.Join("/work", args[1]))
			fmt.Printf("file=%q err=%v\n", string(b), err)
		}
		return 0
	case "marker":
		b, err := os.ReadFile(filepath.Join(jailAuth, "login-marker"))
		fmt.Printf("marker=%q err=%v\n", string(b), err)
		return 0
	case "hwrite", "awrite", "arename":
		if len(args) < 3 || (len(args)-1)%2 != 0 {
			fmt.Println(args[0] + ": 引数が足りない")
			return 2
		}
		base := jailHome
		if args[0] != "hwrite" {
			base = jailAuth
		}
		for i := 1; i < len(args); i += 2 {
			dst := filepath.Join(base, args[i])
			err := os.MkdirAll(filepath.Dir(dst), 0o700)
			switch {
			case err != nil:
			case args[0] == "arename":
				tmp := dst + ".tmp"
				if err = os.WriteFile(tmp, []byte(args[i+1]), 0o600); err == nil {
					err = os.Rename(tmp, dst)
				}
			default:
				err = os.WriteFile(dst, []byte(args[i+1]), 0o600)
			}
			fmt.Printf("%s:%s => %s\n", args[0], args[i], okOrErr(err))
		}
		return 0
	case "hread", "aread":
		base := jailHome
		if args[0] == "aread" {
			base = jailAuth
		}
		for _, name := range args[1:] {
			b, err := os.ReadFile(filepath.Join(base, name))
			if err != nil {
				fmt.Printf("%s:%s => err: %v\n", args[0], name, err)
			} else {
				fmt.Printf("%s:%s => %q\n", args[0], name, string(b))
			}
		}
		return 0
	case "hls":
		fmt.Printf("hls => %s\n", strings.Join(listTree(jailHome), ","))
		return 0
	case "als":
		fmt.Printf("als => %s\n", strings.Join(listTree(jailAuth), ","))
		return 0
	case "env":
		for _, name := range args[1:] {
			fmt.Printf("env:%s => %s\n", name, os.Getenv(name))
		}
		return 0
	case "await":
		if len(args) != 3 {
			fmt.Println("await: 引数が足りない")
			return 2
		}
		fmt.Println("ready")
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if b, err := os.ReadFile(filepath.Join(jailAuth, args[1])); err == nil && string(b) != args[2] {
				fmt.Printf("changed=%q\n", string(b))
				return 0
			}
		}
		fmt.Println("timeout")
		return 1
	case "sigcount":
		return fakeSigCount()
	case "hold":
		fmt.Println("ready")
		select {}
	case "rawtty":
		st, err := saveTermios(0)
		if st == nil {
			fmt.Printf("not-a-tty err=%v\n", err)
			return 1
		}
		raw := rawOf(st.t)
		if err := ioctl(0, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
			fmt.Println("tcsets-error=" + err.Error())
			return 1
		}
		fmt.Println("raw-set")
		if len(args) > 1 && args[1] == "hold" {
			select {}
		}
		os.Stdin.Read(make([]byte, 1))
		return 0
	case "exit":
		if len(args) == 2 {
			if n, err := strconv.Atoi(args[1]); err == nil {
				return n
			}
		}
		return 2
	}
	fmt.Printf("unknown-scenario=%q\n", args[0])
	return 2
}

// okOrErr は、err が無ければ "ok"、あれば "err: 理由"。
func okOrErr(err error) string {
	if err != nil {
		return "err: " + err.Error()
	}
	return "ok"
}

// listTree は、root の下の、通常のファイルと symlink (symlink は "path->先") を、相対 path で、並べて返す。ディレクトリは出さない (中のファイルだけ)。
func listTree(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out = append(out, rel+"->"+target)
		case d.Type().IsRegular():
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// fakeLoopback は、proxy の環境変数を守るクライアントとして、addr に GET する。addr が無ければ、檻の中の loopback にサーバーを立てて、それに GET する。
// 宛先のホストが NO_PROXY (no_proxy) の項目に一致すれば直接、そうでなければ HTTP_PROXY に、絶対形式のリクエストを送る (Bun・Node と同じ)。
func fakeLoopback(args []string) int {
	addr := ""
	if len(args) > 0 {
		addr = args[0]
	} else {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("loopback => err: " + err.Error())
			return 1
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					bufio.NewReader(c).ReadString('\n')
					fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
				}()
			}
		}()
		addr = l.Addr().String()
	}
	host, _, _ := net.SplitHostPort(addr)
	direct := false
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		for _, item := range strings.Split(os.Getenv(key), ",") {
			if item = strings.TrimSpace(item); item != "" && strings.EqualFold(item, host) {
				direct = true
			}
		}
	}
	dial, request, route := addr, "GET / HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n", "direct"
	if !direct {
		dial, route = strings.TrimPrefix(os.Getenv("HTTP_PROXY"), "http://"), "proxy"
		request = "GET http://" + addr + "/ HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n\r\n"
	}
	c, err := net.DialTimeout("tcp", dial, 5*time.Second)
	if err != nil {
		fmt.Println("loopback => err: " + err.Error())
		return 0
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, request)
	line, err := bufio.NewReader(c).ReadString('\n')
	f := strings.Fields(line)
	if err != nil && len(f) < 2 {
		fmt.Println("loopback => err: " + err.Error())
		return 0
	}
	fmt.Printf("loopback => %s %s\n", f[1], route)
	return 0
}

// fakeInfo は、檻の中から見える、起動の状態を出す。
func fakeInfo(args []string) int {
	cwd, _ := os.Getwd()
	var names []string
	for _, kv := range os.Environ() {
		names = append(names, strings.SplitN(kv, "=", 2)[0])
	}
	sort.Strings(names)
	var work []string
	if ents, err := os.ReadDir("/work"); err == nil {
		for _, e := range ents {
			work = append(work, e.Name())
		}
	}
	fmt.Printf("args=%q\ncwd=%s\nhome=%s\nenv=%s\nhttps_proxy=%s\nno_proxy=%s\nwork=%s\n",
		args, cwd, os.Getenv("HOME"), strings.Join(names, ","), os.Getenv("HTTPS_PROXY"), os.Getenv("NO_PROXY"), strings.Join(work, ","))
	return 0
}

// fakeProbe は、操作 1 つを試して、"ok" か、"err: 理由" を返す。
func fakeProbe(op string) string {
	kind, arg, _ := strings.Cut(op, ":")
	var err error
	switch kind {
	case "stat":
		_, err = os.Stat(arg)
	case "write":
		if err = os.WriteFile(arg, []byte("x"), 0o600); err == nil {
			os.Remove(arg)
		}
	case "mnt":
		return mountOptions(arg)
	case "dial":
		var c net.Conn
		if c, err = net.DialTimeout("tcp", arg, 3*time.Second); err == nil {
			c.Close()
		}
	default:
		return "err: 未知の操作"
	}
	if err != nil {
		return "err: " + err.Error()
	}
	return "ok"
}

// fakeConnect は、HTTPS_PROXY に、target の CONNECT を送り、応答の状態コード (数字だけ) を返す。失敗は "err: 理由"。
func fakeConnect(target string) string {
	proxy, ok := strings.CutPrefix(os.Getenv("HTTPS_PROXY"), "http://")
	if !ok {
		return "err: HTTPS_PROXY が http:// で始まらない"
	}
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return "err: " + err.Error()
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return "err: " + err.Error()
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "err: " + err.Error()
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return "err: 応答が正しくない: " + line
	}
	return f[1]
}

// fakeCommit は、/work に file を書いて、git commit する。コミットの ID を出す。
func fakeCommit(file, text, msg string) int {
	if err := os.WriteFile(filepath.Join("/work", file), []byte(text), 0o644); err != nil {
		fmt.Println("write-error=" + err.Error())
		return 1
	}
	for _, args := range [][]string{{"add", "--", file}, {"commit", "-q", "-m", msg}, {"rev-parse", "HEAD"}} {
		out, err := exec.Command("/usr/bin/git", append([]string{"-C", "/work"}, args...)...).CombinedOutput()
		if err != nil {
			fmt.Printf("git-error=%q %v %s\n", args, err, out)
			return 1
		}
		if args[0] == "rev-parse" {
			fmt.Printf("commit=%s", out)
		}
	}
	return 0
}

// fakeSigCount は、最初のシグナルから 1 秒間の、SIGINT と SIGQUIT の数を出す (端末のシグナルが、何回届くかの確認)。
// ready の前に、起動時の SIGINT・SIGQUIT が、無視になっているか (ホストの goro run の無視が、引き継がれたか) も出す。
func fakeSigCount() int {
	fmt.Printf("sigign=%s\n", sigIgnMask())
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGQUIT)
	fmt.Println("ready")
	var ints, quits int
	count := func(s os.Signal) {
		if s == syscall.SIGINT {
			ints++
		} else {
			quits++
		}
	}
	select {
	case s := <-ch:
		count(s)
	case <-time.After(30 * time.Second):
		fmt.Println("timeout")
		return 1
	}
	deadline := time.After(1 * time.Second)
	for {
		select {
		case s := <-ch:
			count(s)
		case <-deadline:
			fmt.Printf("sigint=%d sigquit=%d\n", ints, quits)
			return 0
		}
	}
}

// sigIgnMask は、/proc/self/status の SigIgn (無視しているシグナルの mask) の値。
func sigIgnMask() string {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "SigIgn:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "?"
}

// mountOptions は、/proc/self/mountinfo から、mount 先が path の mount が ro か rw かを返す (無ければ "none")。
// 書き込みを試すより確かだ: 実行中のファイルへの書き込みは、rw でも ETXTBSY で失敗する。
func mountOptions(path string) string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "err: " + err.Error()
	}
	res := "none"
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 5 && f[4] == path { // 同じ場所に重ねた mount は、最後のものが見える
			res = strings.Split(f[5], ",")[0]
		}
	}
	return res
}

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
)

// 檻の中では、テストバイナリが、実際の goro (/opt/goro/goro) と、偽の claude (/opt/claude/claude) の代わりに動く:
// bwrap は、起動するコマンドを、その path を argv[0] にして実行するので、名前で選ぶ。init は、テストバイナリの
// TestMain より先に走る。syscall.Exit は、-race のバイナリの、終了時の 1 秒の待ち (atexit_sleep_ms) を避ける
// (檻に環境変数は渡らないので、GORACE では避けられない)。
func init() {
	switch filepath.Base(os.Args[0]) {
	case "goro":
		syscall.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
	case "claude":
		syscall.Exit(fakeClaude(os.Args[1:]))
	}
}

// fakeClaude は、偽の claude。最初の引数が、場面の名前で、結果を、標準出力に "キー=値" か "操作 => 結果" の行で出す
// (goro run は、標準入出力を、檻の中の claude に直結する)。場面は次の通り。
//
//	auth ...              --login のときの起動 (claude auth login)。引数と環境を出し、HOME にログインの目印を作る
//	info                  引数・作業ディレクトリ・HOME・環境変数の名前・/work の中身を出す
//	probe OP...           OP (stat:PATH・write:PATH・dial:ADDR・mnt:PATH) を試して、結果を出す。mnt は、PATH の mount が ro か rw か
//	connect TARGET...     HTTPS_PROXY へ、TARGET の CONNECT を送り、応答の状態コードを出す
//	commit FILE TEXT MSG  /work に FILE を書いて、git commit する
//	gitlog FILE           /work のコミットの一覧と、FILE の中身を出す
//	marker                HOME のログインの目印を読む
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
	case "auth":
		if err := os.WriteFile("/home/goro/login-marker", []byte("logged-in\n"), 0o600); err != nil {
			fmt.Println("marker-write-error=" + err.Error())
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
		b, err := os.ReadFile("/home/goro/login-marker")
		fmt.Printf("marker=%q err=%v\n", string(b), err)
		return 0
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
	fmt.Printf("args=%q\ncwd=%s\nhome=%s\nenv=%s\nhttps_proxy=%s\nwork=%s\n",
		args, cwd, os.Getenv("HOME"), strings.Join(names, ","), os.Getenv("HTTPS_PROXY"), strings.Join(work, ","))
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

//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// fakeRelayUpstream は、檻の中の偽の opencode (goronation init --relay-control の子): 標準出力は init が読み捨てる pipe なので、記録はすべて標準エラー出力へ出す。
//   - 127.0.0.1:PORT で待ち受け、要求ごとに "REQ <要求の先頭と本文を strconv.Quote したもの>" を出す。
//   - GET /api/event は SSE (100 ミリ秒ごとにイベント。接続が閉じられたら "SSE-CLOSED" を出す)。それ以外は、200 の "ok"。
//   - 標準入力が EOF になったら終わる (serve --stdio と同じ)。
//   - 第 2 引数が spoof なら、子 (spoof-child) に、自分以外のすべてのプロセス (init を含む) への奪取を試させ、結果を出す。さらに、自分 (偽の opencode) が、
//     unix domain socket の fd を 1 つも持たないことを "child:unix-sockets-in-agent => N" で出す (control の socket が、子に継承されていない)。
func fakeRelayUpstream(args []string) int {
	realOut := os.Stdout  // init が読む、起動の証明の 1 行は、ここへ (ADR 0029)
	os.Stdout = os.Stderr // 以後の記録は標準エラー出力へ
	if len(args) < 1 {
		return 2
	}
	port, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	mode := ""
	if len(args) > 1 {
		mode = args[len(args)-1]
	}
	if mode == "exit3" { // 起動の証明の前に終わる
		return 3
	}
	lc := net.ListenConfig{}
	if mode == "reuseport" { // SO_REUSEPORT を付けて待ち受ける (init が、起動を拒否する)
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1) })
		}
	}
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen-error="+err.Error())
		return 1
	}
	go func() {
		io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, "STDIN-EOF")
		os.Exit(0)
	}()
	if mode == "spoof" {
		go func() {
			unixSockets()
			pid1IsInit()
			fakeSpoof(nil)
		}()
	}
	// 環境 (トークンは、子の環境変数 OPENCODE_PASSWORD にだけ来る。旧名は消えている) と、argv にトークンが出ていないことの記録 (init が argv を作る)。
	fmt.Fprintf(os.Stderr, "TOKEN-ENV %s\n", os.Getenv("OPENCODE_PASSWORD"))
	fmt.Fprintf(os.Stderr, "LEGACY-ENV %q\n", os.Getenv("OPENCODE_SERVER_PASSWORD"))
	fmt.Fprintf(os.Stderr, "UPSTREAM-READY %d\n", port)
	fmt.Fprintf(os.Stderr, "ARGV %q\n", os.Args)
	st, _ := os.ReadFile("/proc/self/status") // landlock-exec の下で起動されたか (no_new_privs・seccomp)
	for _, l := range strings.Split(string(st), "\n") {
		if strings.HasPrefix(l, "NoNewPrivs:") || strings.HasPrefix(l, "Seccomp:") {
			fmt.Fprintf(os.Stderr, "SANDBOX %s\n", strings.Join(strings.Fields(l), " "))
		}
	}
	switch mode {
	case "noproof": // 起動の証明を出さない (init が、要求を受け付けないこと・期限で失敗することの確認)
		select {}
	case "wrongurl":
		fmt.Fprintf(realOut, "{\"url\":\"http://127.0.0.1:%d\"}\n", port+1)
	case "notjson":
		fmt.Fprintf(realOut, "http://127.0.0.1:%d\n", port)
	case "longline":
		fmt.Fprintf(realOut, "%s\n", strings.Repeat("a", 4096))
	case "extrakeys": // url が合っていて、ほかのキーがあっても、通る (url だけを見る)
		fmt.Fprintf(realOut, "{\"url\":\"http://127.0.0.1:%d\",\"x\":1}\n", port)
	default:
		fmt.Fprintf(realOut, "{\"url\":\"http://127.0.0.1:%d\"}\n", port)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return 0
		}
		go fakeRelayServe(c)
	}
}

// unixSockets は、自分が持つ fd のうち、unix domain socket の数を出す (getsockname の型で数える。/proc/net/unix は、別の netns で作られた socket
// (ホストが作った control) を載せないので、使えない)。
func unixSockets() {
	n := 0
	ents, _ := os.ReadDir("/proc/self/fd")
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if sa, err := syscall.Getsockname(fd); err == nil {
			if _, ok := sa.(*syscall.SockaddrUnix); ok {
				n++
			}
		}
	}
	fmt.Fprintf(os.Stderr, "child:unix-sockets-in-agent => %d\n", n)
}

// pid1IsInit は、PID 1 が goronation init (--relay-control つき) か (bwrap が PID 1 のまま control を持っていないか) を出す。
// /proc/1/cmdline は同じ uid に読める。
func pid1IsInit() {
	b, _ := os.ReadFile("/proc/1/cmdline")
	args := strings.Split(string(b), "\x00")
	fmt.Fprintf(os.Stderr, "child:pid1-init => %t\n", slices.Contains(args, "init") && slices.Contains(args, "--relay-control"))
}

func fakeRelayServe(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	var head strings.Builder
	cl := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		head.WriteString(line)
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":"); ok && strings.EqualFold(k, "content-length") {
			cl, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if line == "\r\n" {
			break
		}
	}
	body := make([]byte, cl)
	if _, err := io.ReadFull(br, body); err != nil {
		return
	}
	fmt.Fprintln(os.Stderr, "REQ "+strconv.Quote(head.String()+string(body)))
	if strings.HasPrefix(head.String(), "GET /api/event") {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n")
		go func() {
			io.Copy(io.Discard, c)
		}()
		for i := 0; i < 50; i++ {
			if _, err := fmt.Fprintf(c, "data: event-%d\n\n", i); err != nil {
				fmt.Fprintln(os.Stderr, "SSE-CLOSED")
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		return
	}
	io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
}

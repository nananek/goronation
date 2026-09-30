//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
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
	os.Stdout = os.Stderr // init は標準出力を読み捨てる。記録は標準エラー出力へ
	if len(args) < 1 {
		return 2
	}
	port, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen-error="+err.Error())
		return 1
	}
	go func() {
		io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, "STDIN-EOF")
		os.Exit(0)
	}()
	if len(args) > 1 && args[1] == "spoof" {
		go func() {
			unixSockets()
			fakeSpoof(nil)
		}()
	}
	fmt.Fprintf(os.Stderr, "UPSTREAM-READY %d\n", port)
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

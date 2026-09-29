//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// stdioPair は、chat セッション (goronation serve --chat) の、エージェントの標準入力・標準出力になる、Unix ドメインの socketpair 2 本
// (入力用と出力用は別の socket。片方の fd を、エージェントの子が継承しても、もう片方の向きには、書けない・読めない)。
//
// pty も pipe も使わない理由 (承認フローの完全性。ADR 0010 の B3): pipe では、檻の中のプロセスが /proc/<pid>/fd/{0,1} を開き直して、
// 偽のフレーム (偽の result・control_cancel_request・can_use_tool) や、偽の control_response (承認) を書ける。socket は、開き直すと
// ENXIO で断られ、名前の無い socketpair には connect もできない。ただし、socket だけでは、同じ uid のプロセスが pidfd_getfd(2)・
// ptrace(2)・/proc/<pid>/mem で、fd を奪う・メモリを書き換えられる。そちらは、対象を dumpable=0 にして断る
// (goronation init --non-dumpable・chat_exe_linux.go)。これらが塞がっていることは、chat_stdio_bwrap_linux_test.go が確かめる。
type stdioPair struct {
	// AgentIn・AgentOut は、檻のエージェントの標準入力・標準出力に渡す、socketpair のそれぞれ片方。檻を起動した後、呼び手が閉じる。
	AgentIn, AgentOut *os.File
	// In は、ホストがエージェントの入力を書く側 (CloseWrite で、エージェントの標準入力を EOF にする)。Out は、エージェントの出力を読む側。
	In, Out *net.UnixConn
}

// newStdioPair は、stdioPair を作る。どの fd も close-on-exec (檻の bwrap へは、Cmd の Stdin・Stdout で渡すものだけが届く)。
func newStdioPair() (*stdioPair, error) {
	p := &stdioPair{}
	var err error
	if p.AgentIn, p.In, err = socketpairConn(); err != nil {
		return nil, err
	}
	if p.AgentOut, p.Out, err = socketpairConn(); err != nil {
		p.AgentIn.Close()
		p.In.Close()
		return nil, err
	}
	return p, nil
}

// socketpairConn は、socketpair を作り、片方を *os.File (檻に渡す側)、もう片方を *net.UnixConn (ホストの側) にして返す。
func socketpairConn() (*os.File, *net.UnixConn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair を作れない: %w", err)
	}
	agent := os.NewFile(uintptr(fds[0]), "chat-agent-stdio")
	hostFile := os.NewFile(uintptr(fds[1]), "chat-host-stdio")
	defer hostFile.Close() // FileConn は複製を作る
	c, err := net.FileConn(hostFile)
	if err != nil {
		agent.Close()
		return nil, nil, fmt.Errorf("socketpair をホスト側の接続にできない: %w", err)
	}
	return agent, c.(*net.UnixConn), nil
}

// Close は、すべての fd を閉じる。
func (p *stdioPair) Close() {
	p.AgentIn.Close()
	p.AgentOut.Close()
	p.In.Close()
	p.Out.Close()
}

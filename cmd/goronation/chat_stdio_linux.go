//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// stdioPair は、chat セッション (goronation serve --chat) の、エージェントの標準入出力になる、Unix ドメインの socketpair。
//
// pty も pipe も使わない理由 (承認フローの完全性。ADR 0010 の B3): 標準出力が pipe の間、檻の中のプロセスは、/proc/<pid>/fd/1 を
// 開き直して、偽のフレーム (偽の result・control_cancel_request・can_use_tool) を書ける。socket は、/proc/<pid>/fd/N を open(2) すると
// ENXIO で断られる。名前の無い socketpair には connect もできず、fd を得るには、継承するか、pidfd_getfd(2)・SCM_RIGHTS が要る
// (bwrap の檻の中で、これらが塞がっていることは、chat_stdio_bwrap_linux_test.go が確かめる)。
type stdioPair struct {
	// Agent は、檻の中のエージェントの標準入力・標準出力 (両方を同じ fd にする) と標準エラー出力に渡す、socketpair の片方。
	// 檻を起動した後、呼び手が閉じる (ホストの側に、この複製は要らない)。
	Agent *os.File
	// Host は、ホストの側 (エージェントの出力を読み、入力を書く)。CloseWrite で、エージェントの標準入力だけを閉じられる。
	Host *net.UnixConn
}

// newStdioPair は、stdioPair を作る。両方の fd は close-on-exec (檻の bwrap へは、Cmd の Stdin・Stdout で渡すものだけが届く)。
func newStdioPair() (*stdioPair, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair を作れない: %w", err)
	}
	agent := os.NewFile(uintptr(fds[0]), "chat-agent-stdio")
	hostFile := os.NewFile(uintptr(fds[1]), "chat-host-stdio")
	defer hostFile.Close() // FileConn は複製を作る
	c, err := net.FileConn(hostFile)
	if err != nil {
		agent.Close()
		return nil, fmt.Errorf("socketpair をホスト側の接続にできない: %w", err)
	}
	return &stdioPair{Agent: agent, Host: c.(*net.UnixConn)}, nil
}

// Close は、両側を閉じる。
func (p *stdioPair) Close() {
	p.Agent.Close()
	p.Host.Close()
}

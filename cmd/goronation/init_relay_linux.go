//go:build linux

package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
)

// relayRun は、--relay-control のときの init の追加の状態 (PR② は、子の起動と、トークンの渡し方を持たない: PR③)。
type relayRun struct {
	ctl   *net.UnixConn
	relay *requestRelay
	// 子の標準入力の pipe の、init が持つ書き側 (init が死ぬと閉じ、子 (opencode serve --stdio) が終わる)。
	stdinW   *os.File
	childIn  *os.File // 子に渡す側 (起動後に閉じる)
	stdoutR  *os.File // 子の標準出力を読む側 (PR③ が、{"url":…} の確認に使う。ここでは捨てる)
	childOut *os.File
}

// prepareRelay は、--relay-control の準備をする: init の標準入力 (ホストとの socketpair) を control にし、子の標準入出力を、init が別に作る
// pipe に差し替える (init の 0・1 を、子に継承させない)。トークンは init が作り、上流への Authorization にだけ使う (ADR 0020: 子には、
// PR③ が、環境変数で渡す)。
func prepareRelay(cmd *exec.Cmd, port int) (*relayRun, error) {
	c, err := net.FileConn(os.Stdin) // 複製を作る (os.NewFile(0) は、GC で fd 0 を閉じうる)
	if err != nil {
		return nil, fmt.Errorf("標準入力が socket ではない (control にできない): %w", err)
	}
	ctl, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, fmt.Errorf("標準入力が unix domain socket ではない")
	}
	tok, err := newRelayToken()
	if err != nil {
		ctl.Close()
		return nil, err
	}
	r := &relayRun{ctl: ctl, relay: newRequestRelay(port, func() string { return tok })}
	cin, stdinW, err := os.Pipe()
	if err != nil {
		ctl.Close()
		return nil, err
	}
	stdoutR, cout, err := os.Pipe()
	if err != nil {
		ctl.Close()
		cin.Close()
		stdinW.Close()
		return nil, err
	}
	r.stdinW, r.childIn, r.stdoutR, r.childOut = stdinW, cin, stdoutR, cout
	cmd.Stdin, cmd.Stdout = cin, cout
	return r, nil
}

// started は、子を起動した後に呼ぶ: 子に渡した側を閉じ、control の受信を始め、子の標準出力を読み捨てる。
// control が閉じられた (ホストが終わった・OnStop) ら、onGone を呼ぶ。
func (r *relayRun) started(onGone func()) {
	r.childIn.Close()
	r.childOut.Close()
	go io.Copy(io.Discard, r.stdoutR)
	go func() {
		serveControl(r.ctl, r.relay.accept)
		onGone()
	}()
}

// close は、init が終わるときの後始末。
func (r *relayRun) close() {
	r.ctl.Close()
	r.stdinW.Close()
	r.stdoutR.Close()
}

// newRelayToken は、32 バイトの乱数の base64url (パディングなし)。
func newRelayToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("乱数を得られない: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// signalTerm は、子に SIGTERM を送る。
func signalTerm(p *os.Process) { p.Signal(syscall.SIGTERM) }

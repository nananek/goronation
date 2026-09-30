//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// relayRun は、--relay-control のときの init の追加の状態 (PR② は、子の起動と、トークンの渡し方を持たない: PR③)。
type relayRun struct {
	ctl   *net.UnixConn
	relay *requestRelay
	// 子の標準入力の pipe の、init が持つ書き側 (init が死ぬと閉じ、子 (opencode serve --stdio) が終わる)。
	stdinW   *os.File
	childIn  *os.File // 子に渡す側 (起動後に閉じる)
	stdoutR  *os.File // 子の標準出力を読む側 (prove が、起動の証明の 1 行を読む。そのあとは読み捨てる)
	childOut *os.File
	token    string // この起動だけのトークン (init のメモリと、子の環境変数にだけ置く。ADR 0020)
	pidfd    int    // 子の pidfd (生存確認用。attach が、起動の時に取る)。取れていなければ -1
}

// prepareRelay は、--relay-control の準備をする: init の標準入力 (ホストとの socketpair) を control にし、子の標準入出力を、init が別に作る
// pipe に差し替える (init の 0・1 を、子に継承させない)。トークンは init が作り、上流への Authorization と、子の環境変数に使う (ADR 0020)。
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
	r := &relayRun{ctl: ctl, relay: newRequestRelay(port, func() string { return tok }), token: tok, pidfd: -1}
	r.relay.alive = r.childAlive
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

// started は、起動の証明のあとに呼ぶ: control の受信を始める (それまでは、control を読まない = 要求を受け付けない)。
// control が閉じられた (ホストが終わった・OnStop) ら、onGone を呼ぶ。
func (r *relayRun) started(onGone func()) {
	go func() {
		serveControl(r.ctl, r.relay.accept)
		onGone()
	}()
}

// close は、init が終わるときの後始末。
func (r *relayRun) close() {
	if r.pidfd >= 0 {
		syscall.Close(r.pidfd)
	}
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

// relayKillGrace は、control が閉じられてから、SIGTERM を無視する子を SIGKILL で止めるまでの猶予。
const relayKillGrace = 5 * time.Second

// stop は、要求の受け付けを止める (子が死んだとき。control を閉じ、検査中・接続前の要求も、上流に繋がず閉じる)。
func (r *relayRun) stop() {
	r.relay.stop()
	r.ctl.Close()
}

// signalTerm・signalKill は、子に SIGTERM・SIGKILL を送る。
func signalTerm(p *os.Process) { p.Signal(syscall.SIGTERM) }
func signalKill(p *os.Process) { p.Signal(syscall.SIGKILL) }

// relayProofTimeout は、子の起動の証明 ({"url":…} の 1 行) を待つ上限 (ADR 0029。テストで縮めるので var)。
var relayProofTimeout = 30 * time.Second

// relayProofMax は、起動の証明の 1 行の上限 (バイト)。
const relayProofMax = 1024

// attach は、子の起動 (cmd.Start) に、pidfd を取らせる (子の生存確認。ADR 0029 の L1)。Start の後、r.pidfd に入る。
// pidfd は、子が回収される (wait4) までは、その子を指す (pid の再利用で、別のプロセスを指さない)。
func (r *relayRun) attach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.PidFD = &r.pidfd
}

// childAlive は、子が生きているか (pidfd が「終了」を示していないか)。pidfd が無い・poll の失敗は、生きていない扱い (fail closed)。
func (r *relayRun) childAlive() bool {
	if r.pidfd < 0 {
		return false
	}
	return pidfdAlive(r.pidfd)
}

// pidfdAlive は、pidfd の指すプロセスが、まだ終わっていないか。終わると、pidfd は読める (POLLIN) 状態になる。
func pidfdAlive(fd int) bool {
	pfd := struct {
		fd      int32
		events  int16
		revents int16
	}{fd: int32(fd), events: 1} // POLLIN
	var zero syscall.Timespec
	for {
		n, _, e := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&pfd)), 1, uintptr(unsafe.Pointer(&zero)), 0, 0, 0)
		runtime.KeepAlive(&pfd)
		switch {
		case e == syscall.EINTR:
			continue
		case e != 0:
			return false
		}
		return n == 0 // 0: 何も起きていない = 生きている
	}
}

// prove は、ADR 0029 の L0: 子が、自分が選んだポート port の bind に成功したことを確かめる。子の標準出力の最初の 1 行が、
// {"url":"http://127.0.0.1:<port>"} でなければ (上限・期限つき)・子が先に終わったら、エラー。そのあと、port に SO_REUSEPORT つきで
// bind できたら (持ち主が SO_REUSEPORT を付けている。別のプロセスが同じポートを共有できる)、エラー。成功したら、標準出力の残りは読み捨てる。
func (r *relayRun) prove(port int, timeout time.Duration) error {
	// 子に渡した側は、起動した今、閉じる (閉じないと、子が死んでも、標準出力が EOF にならない)。
	r.childIn.Close()
	r.childOut.Close()
	br := bufio.NewReaderSize(r.stdoutR, relayProofMax)
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := br.ReadSlice('\n')
		ch <- result{append([]byte(nil), line...), err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-time.After(timeout):
		return fmt.Errorf("%v 以内に、起動の証明が出ない", timeout)
	}
	switch {
	case errors.Is(res.err, io.EOF):
		return errors.New("起動の証明の前に、子が標準出力を閉じた (終わった)")
	case errors.Is(res.err, bufio.ErrBufferFull):
		return fmt.Errorf("起動の証明の行が、%d バイトを超える", relayProofMax)
	case res.err != nil:
		return res.err
	}
	var msg struct {
		URL *string `json:"url"`
	}
	want := "http://127.0.0.1:" + strconv.Itoa(port)
	if err := json.Unmarshal(res.line, &msg); err != nil || msg.URL == nil || *msg.URL != want {
		return fmt.Errorf("起動の証明が %q と一致しない: %q", want, shorten(string(res.line), 120))
	}
	if taken, err := portSharable(port); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("127.0.0.1:%d の持ち主が SO_REUSEPORT を付けている (別のプロセスが同じポートを共有できる)", port)
	}
	go io.Copy(io.Discard, br)
	return nil
}

// portSharable は、127.0.0.1:port に、SO_REUSEPORT つきで bind できるか (できたら、すぐ閉じる)。EADDRINUSE (持ち主が付けていない) なら false。
// それ以外の失敗は、確かめられなかったので、エラー (fail closed)。
func portSharable(port int) (bool, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1) }); err != nil {
			return err
		}
		return serr
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err == nil {
		l.Close()
		return true, nil
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return false, nil
	}
	return false, fmt.Errorf("SO_REUSEPORT の自己検査ができない: %w", err)
}

// soReusePort は、SO_REUSEPORT (Linux は 15。syscall に名前が無い arch がある)。
const soReusePort = 15

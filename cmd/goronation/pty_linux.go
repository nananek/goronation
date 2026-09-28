package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// pty 中継: goronation run (ホスト) は、檻専用の pty を作り、master を持つ。ローカルの実端末 (raw モードにする) と
// master の間を、そのままバイト列で中継する (goronation は、その中身を解釈・模倣しない)。エージェントは、pty の
// slave を、自分の標準入出力として持つ (goronation init が、エージェントを起動するときの SysProcAttr で、--set-ctty
// なら Setsid・Setctty を指定する。エージェント自身の fork の直後・exec の前に、エージェント自身が新しい
// セッションの leader になり、その制御端末にする)。これにより、エージェントは、ホストの実端末に直結しなく
// なる (TIOCSTI は、自分専用の pty にしか効かない)。

// ctlFile は、f の fd で fn を実行する (f.Fd() で fd を取ると、その fd が永久に blocking になり、Close による
// 読みの中断や SetReadDeadline が効かなくなる。SyscallConn 経由なら、fd は non-blocking のまま)。
func ctlFile(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// isTTYFile は、f が端末か (TCGETS が ENOTTY にならないか。SyscallConn 経由なので、f を blocking にしない)。
func isTTYFile(f *os.File) bool {
	return ctlFile(f, func(fd int) error {
		var t syscall.Termios
		return ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t))
	}) == nil
}

// openHostPty は、/dev/ptmx を開いて鍵を外し、対応する slave (/dev/pts/N) を開く。呼び手が、両方を閉じる
// (bwrap へ slave を渡した後は、この関数を呼んだ側の slave の複製は、goronation run 側で閉じてよい)。
func openHostPty() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("/dev/ptmx を開けない: %w", err)
	}
	var unlock int32
	if err := ctlFile(m, func(fd int) error { return ioctl(fd, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)) }); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("pty の鍵を外せない: %w", err)
	}
	var n uint32
	if err := ctlFile(m, func(fd int) error { return ioctl(fd, syscall.TIOCGPTN, unsafe.Pointer(&n)) }); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("pty の番号を取れない: %w", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("pty の slave を開けない: %w", err)
	}
	return m, s, nil
}

// winsize は、TIOCGWINSZ・TIOCSWINSZ が使う struct winsize (asm-generic/termios.h)。標準ライブラリに型が無いので、ここに定義する。
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// getWinsize は、f (端末) の今の大きさ。
func getWinsize(f *os.File) (winsize, error) {
	var ws winsize
	err := ctlFile(f, func(fd int) error { return ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) })
	return ws, err
}

// setWinsize は、f (端末) の大きさを ws にする。pty の master・slave のどちらに対して呼んでも、両方に効く
// (1 組で 1 つの大きさを共有する)。slave 側の、その時点のフォアグラウンドの process group に SIGWINCH が届く
// (大きさが変わったときだけ。kernel が行う。goronation は、明示的にシグナルを送らない)。
func setWinsize(f *os.File, ws winsize) error {
	return ctlFile(f, func(fd int) error { return ioctl(fd, syscall.TIOCSWINSZ, unsafe.Pointer(&ws)) })
}

// ptyRelay は、ホストの実端末 (stdin・stdout。あらかじめ raw モードにしておくのは呼び手の責任) と、pty の
// master の間を中継する goroutine と、SIGWINCH を master の大きさに反映する goroutine を持つ。
type ptyRelay struct {
	master   *os.File
	stdin    *os.File
	winCh    chan os.Signal
	done     chan struct{}
	closeOne sync.Once
	wg       sync.WaitGroup // master→stdout の中継と、resize の監視 (stdin→master は待たない。下の stop を参照)
}

// startPtyRelay は、master の今の大きさを stdin (実端末) に合わせてから、stdin→master・master→stdout の中継と、
// SIGWINCH での大きさの反映を始める。呼び手は、檻が終わったら stop を呼ぶ。
func startPtyRelay(stdin, stdout, master *os.File) *ptyRelay {
	if ws, err := getWinsize(stdin); err == nil {
		setWinsize(master, ws) // 失敗しても、初回の大きさが合わないだけ (致命的ではない)。エージェント自身が resize で追随できる
	}
	r := &ptyRelay{master: master, stdin: stdin, winCh: make(chan os.Signal, 4), done: make(chan struct{})}
	signal.Notify(r.winCh, syscall.SIGWINCH)
	// stdin→master: 中継を止めるとき、SetReadDeadline で読みを中断する (stop を参照)。プロセスが終わるまで
	// ブロックしたままでも安全 (goroutine リークにはなるが、os.Exit で一緒に終わる)。WaitGroup には入れない。
	go func() { io.Copy(master, stdin) }()
	r.wg.Add(2)
	go func() { defer r.wg.Done(); io.Copy(stdout, master) }()
	go func() { defer r.wg.Done(); r.watchResize() }()
	return r
}

// watchResize は、SIGWINCH のたびに、master の大きさを、そのときの stdin (実端末) の大きさに合わせる。
func (r *ptyRelay) watchResize() {
	for {
		select {
		case <-r.winCh:
			if ws, err := getWinsize(r.stdin); err == nil {
				setWinsize(r.master, ws)
			}
		case <-r.done:
			return
		}
	}
}

// stop は、中継を止める。stdin の読み取りを SetReadDeadline で中断し (実端末が対応していなければ、次にホストが
// 何か打つまで、その goroutine は残るが、プロセスはこの後すぐ終わるので実害は無い)、master を閉じて
// master→stdout の中継を終わらせ、resize の監視も止めて、その 2 つの終了を待つ。master は、呼び手がすでに
// 別の目的で保持していなければ、これで閉じる (二重 Close はしない)。
func (r *ptyRelay) stop() {
	_ = r.stdin.SetReadDeadline(time.Now())
	signal.Stop(r.winCh)
	r.closeOne.Do(func() { close(r.done) })
	r.master.Close()
	r.wg.Wait()
}

//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

// sigWatch は、goro run が受けるシグナルの扱い。
//
//   - 檻を起動する前 (clone の作成など): SIGINT・SIGTERM・SIGHUP のどれでも、取り消して、後始末に進む。
//   - 檻の中では (enterCage の後): 端末のシグナル (Ctrl-C の SIGINT・SIGQUIT・リサイズの SIGWINCH) は、フォアグラウンドの
//     process group 全体に届き、檻の中の claude も直接受ける。ホストの goro run は SIGINT と SIGQUIT を無視し、claude の中断で、
//     ホスト側 (bwrap を含む) が落ちて、檻が死なないようにする。無視の設定は、起動する bwrap に引き継がれる
//     (goro init が、子の claude を起動する前に、自分の側で戻す)。SIGTERM と SIGHUP は、檻を止めて (取り消して)、後始末に進む。
type sigWatch struct {
	ch     chan os.Signal
	cancel context.CancelFunc
	inCage atomic.Bool
	done   chan struct{}

	mu  sync.Mutex
	got os.Signal // 取り消しの原因になったシグナル
}

// watchSignals は、SIGINT・SIGTERM・SIGHUP を受けたら cancel を呼ぶようにする。
func watchSignals(cancel context.CancelFunc) *sigWatch {
	w := &sigWatch{ch: make(chan os.Signal, 4), cancel: cancel, done: make(chan struct{})}
	signal.Notify(w.ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go w.loop()
	return w
}

func (w *sigWatch) loop() {
	for {
		select {
		case s := <-w.ch:
			if s == syscall.SIGINT && w.inCage.Load() {
				continue // enterCage の前に届いて、ためられていた分
			}
			w.mu.Lock()
			if w.got == nil {
				w.got = s
			}
			w.mu.Unlock()
			w.cancel()
		case <-w.done:
			return
		}
	}
}

// enterCage は、檻を起動する直前に呼ぶ: SIGINT と SIGQUIT を無視する。
func (w *sigWatch) enterCage() {
	w.inCage.Store(true)
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
}

// received は、取り消しの原因になったシグナル (無ければ nil)。
func (w *sigWatch) received() os.Signal {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.got
}

// stop は、シグナルの扱いを元に戻す。signal.Reset は、Notify だけを戻し、Ignore は戻さない: SIGINT・SIGQUIT は、
// 一度 Notify して止めて、無視でなくする (Go の handler が入り、子には、既定の扱いで渡る)。
func (w *sigWatch) stop() {
	signal.Stop(w.ch)
	undo := make(chan os.Signal, 1)
	signal.Notify(undo, syscall.SIGINT, syscall.SIGQUIT)
	signal.Stop(undo)
	close(w.done)
}

// exitCodeForSignal は、シグナルで止まったときの、goro run の終了コード (シェルの慣習の 128 + 番号)。
func exitCodeForSignal(s os.Signal) int {
	if sig, ok := s.(syscall.Signal); ok {
		return 128 + int(sig)
	}
	return 1
}

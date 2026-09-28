//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// watchServeSignals は、SIGINT・SIGTERM・SIGHUP を受けたら cancel を呼ぶ。goronation run の sigWatch
// (enterCage でホストの制御端末を檻と共有する前提の仕組み) とは別で、goronation serve は檻と端末を共有し
// ない (端末ビューは WebSocket 越し) ので、単純に ctx を取り消すだけでよい。呼び手は、ctx の取り消しを
// 見て、HTTP サーバー (Shutdown)・檻 (bwrap.Start に渡した ctx 経由)・egress (proxy.Close) を、通常の
// graceful shutdown として片付ける。stop は、シグナルの扱いを元に戻す。
func watchServeSignals(cancel context.CancelFunc) (stop func()) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			cancel()
		case <-done:
		}
	}()
	return func() {
		close(done)
		signal.Stop(ch)
	}
}

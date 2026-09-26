package egress

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// relay は、client (先読みしたバイトは cr に残っている) と up の間を、両方向に中継する。
// 片方向の EOF は、その向きの書き込み側だけを閉じる (半クローズ。CloseWrite が無い conn は、両方を閉じる)。
// EOF 以外の error・idle の期限・ctx の終了は、両方を閉じる。
// idle は、両方向をあわせた無通信の期限で、どちらかが動いている間は延びる。
func relay(ctx context.Context, client net.Conn, cr io.Reader, up net.Conn, idle time.Duration) {
	var once sync.Once
	stop := func() {
		once.Do(func() {
			client.Close()
			up.Close()
		})
	}
	timer := time.AfterFunc(idle, stop)
	defer timer.Stop()
	defer context.AfterFunc(ctx, stop)()
	touch := func() { timer.Reset(idle) }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pipe(up, cr, touch, stop)
	}()
	go func() {
		defer wg.Done()
		pipe(client, up, touch, stop)
	}()
	wg.Wait()
	stop()
}

// pipe は、src から dst へ、EOF まで写す。読み書きのたびに touch を呼ぶ。
func pipe(dst net.Conn, src io.Reader, touch, abort func()) {
	buf := make([]byte, 32<<10)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			touch()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				abort()
				return
			}
			touch()
		}
		if rerr != nil {
			if rerr != io.EOF {
				abort()
				return
			}
			if cw, ok := dst.(interface{ CloseWrite() error }); !ok || cw.CloseWrite() != nil {
				abort()
			}
			return
		}
	}
}

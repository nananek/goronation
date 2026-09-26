package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	// maxConns は、リレーが同時に中継する接続数の上限。
	maxConns = 128

	// dialTimeout は、上流の UDS への接続の待ち時間の上限。
	dialTimeout = 10 * time.Second
)

// relay は、TCP の接続を、上流の Unix ドメインソケットへ 1 対 1 で中継する。
// 出力は一切しない (子の stdio と同じ端末に、ログを混ぜないため)。
type relay struct {
	upstream string
	slots    chan struct{} // 中継中の接続ごとに 1 つ埋まる。容量が上限
}

func newRelay(upstream string, limit int) *relay {
	return &relay{upstream: upstream, slots: make(chan struct{}, limit)}
}

// serve は、l で接続を受け付けて中継する。l が閉じられたら戻る。
// 上限に達しているときに来た接続は、受けてすぐに閉じる (待たせない)。
func (r *relay) serve(l net.Listener) {
	var delay time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// fd の枯渇などの一時的な失敗。空回りしないよう、間を置いて続ける。
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			time.Sleep(delay)
			continue
		}
		delay = 0
		select {
		case r.slots <- struct{}{}:
			go func() {
				defer func() { <-r.slots }()
				r.handle(c)
			}()
		default:
			c.Close()
		}
	}
}

// handle は、client を上流へ中継し、両方が終わったら閉じる。上流に繋がらなければ、client を閉じるだけ。
func (r *relay) handle(client net.Conn) {
	defer client.Close()
	up, err := net.DialTimeout("unix", r.upstream, dialTimeout)
	if err != nil {
		return
	}
	defer up.Close()
	pipe(client, up)
}

// pipe は、a と b の間を双方向に中継し、両方向が終わるまで戻らない。
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		half(b, a)
	}()
	go func() {
		defer wg.Done()
		half(a, b)
	}()
	wg.Wait()
}

// half は src から dst へ、src が EOF になるまで写す。
// EOF なら、dst の書き側だけを閉じる (半クローズ。逆向きは続く)。
// 読み書きの失敗なら、両方を閉じて、逆向きも終わらせる。
func half(dst, src net.Conn) {
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		src.Close()
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	dst.Close()
}

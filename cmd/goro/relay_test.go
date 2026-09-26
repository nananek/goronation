package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startRelay は、127.0.0.1 の空きポートで relay を起こし、そのアドレスを返す。
func startRelay(t *testing.T, upstream string, limit int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go newRelay(upstream, limit).serve(l)
	return l.Addr().String()
}

func dialTCP(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(testTimeout))
	t.Cleanup(func() { c.Close() })
	return c.(*net.TCPConn)
}

// exchange は、msg を書いて、同じ長さを読み、echo の応答かどうかを見る。
func exchange(c net.Conn, msg string) error {
	if _, err := c.Write([]byte(msg)); err != nil {
		return err
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != msg {
		return fmt.Errorf("応答 %q, want %q", got, msg)
	}
	return nil
}

// requireClosedByRelay は、c が、データを受けずに、relay に閉じられる (EOF か reset) ことを確かめる。
func requireClosedByRelay(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("x")) // 閉じられた後の書き込みの失敗は構わない
	n, err := c.Read(make([]byte, 16))
	if n != 0 {
		t.Fatalf("閉じられるはずの接続から %d バイト読めた", n)
	}
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("接続が閉じられない: %v", err)
	}
}

func TestRelayRoundTrip(t *testing.T) {
	sock := tempSock(t)
	startUpstream(t, sock, echo)
	c := dialTCP(t, startRelay(t, sock, 8))

	// 双方向とも、1 MiB の任意のバイト列 (NUL や 0xff を含む) が、そのまま往復する。
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	go func() {
		if _, err := c.Write(payload); err != nil {
			t.Errorf("書き込み: %v", err)
		}
		c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("往復したバイト列が違う: %d バイト, want %d", len(got), len(payload))
	}
}

func TestRelayHalfCloseFromClient(t *testing.T) {
	sock := tempSock(t)
	startUpstream(t, sock, hostSaw) // EOF を受けるまで、返さない
	c := dialTCP(t, startRelay(t, sock, 8))

	c.Write([]byte("hello"))
	c.CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := hostSawPrefix + "hello"; string(got) != want {
		t.Fatalf("応答 %q, want %q", got, want)
	}
}

func TestRelayHalfCloseFromUpstream(t *testing.T) {
	sock := tempSock(t)
	late := make(chan string, 1)
	startUpstream(t, sock, func(c net.Conn) {
		defer c.Close()
		c.Write([]byte("greeting"))
		c.(*net.UnixConn).CloseWrite()
		b, _ := io.ReadAll(c) // 上流が書き側を閉じた後も、client からの続きを受ける
		late <- string(b)
	})
	c := dialTCP(t, startRelay(t, sock, 8))

	got, err := io.ReadAll(c) // 上流の EOF が、client の EOF になる
	if err != nil || string(got) != "greeting" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
	c.Write([]byte("late-data"))
	c.CloseWrite()
	select {
	case s := <-late:
		if s != "late-data" {
			t.Fatalf("上流が受けた続き = %q", s)
		}
	case <-time.After(testTimeout):
		t.Fatal("上流が client からの続きを受けられない")
	}
}

// client が reset で切れたら、上流が開いたままでも、接続の全体を閉じて、上限の枠を返す。
func TestRelayResetReleasesConnection(t *testing.T) {
	sock := tempSock(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var conns atomic.Int32
	startUpstream(t, sock, func(c net.Conn) {
		defer c.Close()
		if conns.Add(1) == 1 {
			io.Copy(io.Discard, c)
			<-release // EOF の後も、閉じずに開いておく
			return
		}
		echo(c)
	})
	addr := startRelay(t, sock, 1)

	c1 := dialTCP(t, addr)
	c1.Write([]byte("hold"))
	c1.SetLinger(0)
	c1.Close() // RST

	deadline := time.Now().Add(testTimeout)
	for {
		c2 := dialTCP(t, addr)
		err := exchange(c2, "again")
		c2.Close()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reset した接続の枠が返らない: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRelayConcurrent(t *testing.T) {
	const n = 32
	sock := tempSock(t)
	var active atomic.Int32
	startUpstream(t, sock, func(c net.Conn) {
		active.Add(1)
		defer active.Add(-1)
		echo(c)
	})
	addr := startRelay(t, sock, n)

	var sent, done sync.WaitGroup
	release := make(chan struct{})
	sent.Add(n)
	done.Add(n)
	for i := range n {
		go func() {
			defer done.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				sent.Done()
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(testTimeout))
			payload := bytes.Repeat([]byte(fmt.Sprintf("client-%02d;", i)), 8<<10)
			_, werr := c.Write(payload)
			sent.Done()
			if werr != nil {
				t.Errorf("client %d: 書き込み: %v", i, werr)
				return
			}
			<-release // 全員が接続して書き終えるまで、閉じない
			c.(*net.TCPConn).CloseWrite()
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, payload) {
				t.Errorf("client %d: 応答が違う (%d バイト, err=%v)", i, len(got), err)
			}
		}()
	}
	sent.Wait()
	// 上流から見て n 本が同時に開いていること (直列に中継していれば、n にならない)。
	deadline := time.Now().Add(testTimeout)
	for active.Load() != n {
		if time.Now().After(deadline) {
			t.Fatalf("同時に開いた上流の接続 = %d, want %d", active.Load(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	done.Wait()
}

func TestRelayLimit(t *testing.T) {
	sock := tempSock(t)
	var accepted atomic.Int32
	startUpstream(t, sock, func(c net.Conn) {
		accepted.Add(1)
		echo(c)
	})
	addr := startRelay(t, sock, 2)

	c1, c2 := dialTCP(t, addr), dialTCP(t, addr)
	for i, c := range []net.Conn{c1, c2} {
		if err := exchange(c, "hold"); err != nil {
			t.Fatalf("上限内の接続 %d: %v", i+1, err)
		}
	}

	requireClosedByRelay(t, dialTCP(t, addr))
	if got := accepted.Load(); got != 2 {
		t.Errorf("上流が受けた接続 = %d, want 2 (上限を超えた接続は、上流へ繋がない)", got)
	}

	// 枠が空けば、また受け付ける。枠の返却は非同期なので、待つ。
	c1.Close()
	deadline := time.Now().Add(testTimeout)
	for {
		c := dialTCP(t, addr)
		err := exchange(c, "again")
		c.Close()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("枠が空いても受け付けない: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRelayUpstreamUnavailable(t *testing.T) {
	sock := tempSock(t)
	addr := startRelay(t, sock, 8)

	t.Run("path が無い", func(t *testing.T) {
		requireClosedByRelay(t, dialTCP(t, addr))
	})

	t.Run("待ち受けが無い", func(t *testing.T) {
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		l.Close() // socket のファイルだけが残る (ECONNREFUSED)
		requireClosedByRelay(t, dialTCP(t, addr))
	})

	// 上流が来れば、以前の失敗に引きずられず、中継する。
	t.Run("後から上流が起きる", func(t *testing.T) {
		os.Remove(sock)
		startUpstream(t, sock, echo)
		if err := exchange(dialTCP(t, addr), "hello"); err != nil {
			t.Fatal(err)
		}
	})
}

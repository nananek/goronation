package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperArg が最初の引数のとき、テストバイナリは、テストではなく、下のヘルパーとして動く。
// 実プロセスの goro と、その子 (シグナルや環境変数を確かめる役) を、テストバイナリの再実行で用意する。
const helperArg = "__goro_test_helper__"

// helpers は、名前で選ぶテスト用の子プログラム。他のテストファイルが、init で足す。
// "goro" は、dispatch (実際の goro) を動かす特別な名前で、この表には無い。
var helpers = map[string]func(args []string) int{}

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		name, args := os.Args[2], os.Args[3:]
		if name == "goro" {
			os.Exit(dispatch(args, os.Stdout, os.Stderr))
		}
		h, ok := helpers[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "未知のヘルパー: %q\n", name)
			os.Exit(99)
		}
		os.Exit(h(args))
	}
	// -race のバイナリは、終了のたびに 1 秒待つ (atexit_sleep_ms の既定)。ヘルパーとして再実行する子は、待たせない。
	os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	os.Exit(m.Run())
}

// 中継や子の応答を待つ時間の上限。-race と CI の遅さを見込み、正常なら足りる長さにする (失敗は早く分かる)。
const testTimeout = 20 * time.Second

// selfCmd は、テストバイナリを、ヘルパー name として再実行するコマンドの argv を返す。
func selfCmd(t *testing.T, name string, args ...string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{exe, helperArg, name}, args...)
}

// tempSock は、UDS を置く短い path を返す (UDS の path は 108 バイトまで)。
func tempSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "goro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "up.sock")
}

// startUpstream は、path の UDS で待ち受け、接続ごとに handle を別の goroutine で呼ぶ。
func startUpstream(t *testing.T, path string, handle func(net.Conn)) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
}

// echo は、読んだ分をそのまま返し、EOF で閉じる上流。
func echo(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if _, werr := c.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// hostSawPrefix は、hostSaw が返す文字列の接頭辞。
const hostSawPrefix = "host-saw:"

// hostSaw は、EOF まで読み、hostSawPrefix + 読んだ全部を返して閉じる上流 (半クローズが通らないと、返らない)。
func hostSaw(c net.Conn) {
	defer c.Close()
	b, _ := io.ReadAll(c)
	c.Write(append([]byte(hostSawPrefix), b...))
}

func TestDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"引数なし", nil, exitUsage, "使い方: goro"},
		{"未知のサブコマンド", []string{"bogus"}, exitUsage, `未知のサブコマンド: "bogus"`},
		{"--help", []string{"--help"}, 0, "使い方: goro"},
		{"-h", []string{"-h"}, 0, "使い方: goro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := dispatch(tc.args, io.Discard, &stderr); got != tc.code {
				t.Errorf("終了コード = %d, want %d", got, tc.code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr に %q が無い: %q", tc.want, stderr.String())
			}
		})
	}
}

//go:build linux

package session

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// gitBin は、檻の中で実行する git (ホストの /usr を ro で bind したもの)。
const gitBin = "/usr/bin/git"

// gitEnv は、檻の中の git の環境変数 (これだけ。ホストの環境変数は渡らない)。
// GIT_CONFIG_* は、リポジトリの設定より優先する (コマンドラインと同じ扱い): 檻の中でも、fsmonitor と hooks は無効にし、
// 所有者の違うリポジトリでも動くように safe.directory を許す (檻には、守るものが無い)。
var gitEnv = []bwrap.EnvVar{
	{Key: "HOME", Value: "/home/goro"}, {Key: "PATH", Value: "/usr/bin:/bin"}, {Key: "LANG", Value: "C.UTF-8"},
	{Key: "GIT_CONFIG_NOSYSTEM", Value: "1"}, {Key: "GIT_CONFIG_GLOBAL", Value: "/dev/null"}, {Key: "GIT_TERMINAL_PROMPT", Value: "0"},
	{Key: "GIT_CONFIG_COUNT", Value: "3"},
	{Key: "GIT_CONFIG_KEY_0", Value: "safe.directory"}, {Key: "GIT_CONFIG_VALUE_0", Value: "*"},
	{Key: "GIT_CONFIG_KEY_1", Value: "core.fsmonitor"}, {Key: "GIT_CONFIG_VALUE_1", Value: "false"},
	{Key: "GIT_CONFIG_KEY_2", Value: "core.hooksPath"}, {Key: "GIT_CONFIG_VALUE_2", Value: "/dev/null"},
}

// gitSpec は、使い捨ての檻で git を実行する Spec。ネットワークは無く (bwrap の既定)、資格情報も無い。
// /usr は ro、HOME と /tmp は tmpfs (使い捨て)。binds に、この実行が要るものだけを足す。
func (s *Store) gitSpec(binds []bwrap.Bind, args ...string) bwrap.Spec {
	return bwrap.Spec{
		Host: s.host,
		Symlinks: []bwrap.Symlink{
			{Target: "usr/lib", Dst: "/lib"}, {Target: "usr/lib64", Dst: "/lib64"},
			{Target: "usr/bin", Dst: "/bin"}, {Target: "usr/sbin", Dst: "/sbin"},
		},
		Tmpfs: []string{"/tmp", "/home/goro"},
		Binds: append([]bwrap.Bind{{Src: "/usr", Dst: "/usr"}}, binds...),
		Env:   gitEnv,
		Chdir: "/tmp",
		Cmd:   append([]string{gitBin}, args...),
	}
}

// bind は、ホストの path src を、檻の中の dst に見せる Bind。ホストの HOME の下の path (セッションのディレクトリ・
// 元の repo など、このパッケージが作る・利用者が指定した作業用の path) は、InHome を明示する。
func (s *Store) bind(src, dst string, rw bool) bwrap.Bind {
	return bwrap.Bind{Src: src, Dst: dst, RW: rw, InHome: under(src, s.host.Home)}
}

// cloneBinds は、clone の檻が見せるもの: 元の repo を ro で /src に、clone 先を rw で /work に。
func (s *Store) cloneBinds(repo string, sess *Session) []bwrap.Bind {
	return []bwrap.Bind{s.bind(repo, "/src", false), s.bind(sess.Clone, "/work", true)}
}

// workBinds は、clone の中で git を実行する檻が見せるもの: clone を rw で /work に。
func (s *Store) workBinds(sess *Session) []bwrap.Bind {
	return []bwrap.Bind{s.bind(sess.Clone, "/work", true)}
}

// exportBinds は、export の檻が見せるもの: clone を ro で /work に (檻の中で bundle を作るだけで、clone は書き換えない)、
// export/ を rw で /out に。
func (s *Store) exportBinds(sess *Session) []bwrap.Bind {
	return []bwrap.Bind{s.bind(sess.Clone, "/work", false), s.bind(sess.Export, "/out", true)}
}

// runGit は、使い捨ての檻で git args を実行し、終わるのを待つ。失敗したら、出力 (制御文字を除いたもの) を添えて error にする。
func (s *Store) runGit(ctx context.Context, binds []bwrap.Bind, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	spec := s.gitSpec(binds, args...)
	var out boundedBuffer
	spec.Stdout, spec.Stderr = &out, &out
	c, err := bwrap.Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("session: 檻を起動できない: %w", err)
	}
	if err := c.Wait(); err != nil {
		return fmt.Errorf("session: 檻の中の git %s が失敗した: %w\n%s", args[0], err, out.text())
	}
	return nil
}

// maxCapture は、檻の出力を持つ上限。超えた分は捨てる。
const maxCapture = 8 << 10

// boundedBuffer は、書かれた先頭の maxCapture バイトだけを持つ Writer (檻の出力は、敵対入力で、大きくなりうる)。
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p[:min(maxCapture-b.buf.Len(), len(p))]) // buf は、maxCapture を超えないので、引数は 0 以上
	return len(p), nil
}

// text は、持っている出力を、端末に出しても安全な形 (制御文字と、不正な UTF-8 を ? にしたもの。改行とタブは残す) で返す。
func (b *boundedBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (0x80 <= r && r < 0xa0) || r == 0xfffd || r == 0x2028 || r == 0x2029 || (0x202a <= r && r <= 0x202e) || (0x2066 <= r && r <= 0x2069) {
			return '?'
		}
		return r
	}, strings.TrimSpace(b.buf.String()))
}

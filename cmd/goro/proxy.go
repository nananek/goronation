//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nananek/goronation/egress"
)

const (
	// proxySockName・egressLogName・lockName は、run dir の中のファイル名。
	proxySockName = "proxy.sock"
	egressLogName = "egress.log"
	lockName      = "lock"

	// maxSockPath は、UDS の path の長さの上限 (sockaddr_un.sun_path の 108 バイトから、終端の NUL を除く)。
	maxSockPath = 107

	// auditQueue は、監査の行を、ファイルに書く前にためておく数。ためきれない分は捨てる。
	auditQueue = 1024
	// auditRunBudget は、1 回の goro run が、egress.log に書く量の上限 (バイト)。檻が CONNECT を繰り返して、ホストのディスクを
	// 埋めないようにする。
	auditRunBudget = 8 << 20
)

// auditCloseWait は、終了時に、ためた行を書き終えるのを待つ上限 (テストが縮める)。
var auditCloseWait = 2 * time.Second

// auditLog は、egress の監査 (Config.Audit) の書き込み先。Write は、決して待たない: egress は、accept の途中と、Close の中で
// Audit.Write を呼ぶので、書き込みが詰まる (ディスクの停止など) と、accept と Close が止まる。行は、別の goroutine が
// ファイルに書き、ためきれない行と、書く量の上限を超えた行は、捨てて数える。
type auditLog struct {
	mu     sync.Mutex // closed と ch の close を守る (送るのは、待たない)
	closed bool
	ch     chan []byte
	done   chan struct{}

	dropped atomic.Int64
	w       io.Writer
	budget  int64 // 書き込みの goroutine だけが触る
}

// newAuditLog は、w に書く auditLog を作り、書き込みの goroutine を起こす。budget は、w に書く量の上限 (バイト)。
func newAuditLog(w io.Writer, queue int, budget int64) *auditLog {
	a := &auditLog{ch: make(chan []byte, queue), done: make(chan struct{}), w: w, budget: budget}
	go a.run()
	return a
}

func (a *auditLog) run() {
	defer close(a.done)
	for b := range a.ch {
		if int64(len(b)) > a.budget {
			a.dropped.Add(1)
			continue
		}
		if _, err := a.w.Write(b); err != nil {
			a.dropped.Add(1)
			continue
		}
		a.budget -= int64(len(b))
	}
}

// Write は、p (の写し) を、ためる。ためきれなければ、捨てて数える。常に成功を返し、待たない。
func (a *auditLog) Write(p []byte) (int, error) {
	b := slices.Clone(p)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return len(p), nil
	}
	select {
	case a.ch <- b:
	default:
		a.dropped.Add(1)
	}
	return len(p), nil
}

// Close は、ためた行を書き終えるのを、auditCloseWait まで待ち、捨てた行の数 (書き込みに失敗した行を含む) を返す。
// 書き込みが詰まっているときは、待ちきれず、書けなかった分が数に入らない。
func (a *auditLog) Close() int64 {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.ch)
	}
	a.mu.Unlock()
	select {
	case <-a.done:
	case <-time.After(auditCloseWait):
	}
	return a.dropped.Load()
}

// hostProxy は、goro run が、ホスト側で動かす egress: run dir の UDS で待ち受け、監査を egress.log に書く。
type hostProxy struct {
	srv      *egress.Server
	audit    *auditLog
	logFile  *os.File
	logPath  string
	logStart int64 // この起動で書き始める前の、egress.log の大きさ
	lock     *os.File
	sockPath string
	serveErr chan error
}

// startProxy は、runDir (0700 で、作ってある) の UDS で待ち受ける egress を起こす。同じ runDir を、同時に 2 つの goro run が
// 使うことは、ロックで断る (前の起動が残した UDS は、ロックを持てたときだけ消す)。
func startProxy(runDir string, allow []string) (p *hostProxy, err error) {
	sock := filepath.Join(runDir, proxySockName)
	if err := checkSockPath(sock); err != nil {
		return nil, err
	}
	lock, err := lockDir(runDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
	logPath := filepath.Join(runDir, egressLogName)
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("監査ログを開けない: %w", err)
	}
	defer func() {
		if err != nil {
			logFile.Close()
		}
	}()
	fi, err := logFile.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("監査ログ %s が通常のファイルではない (%v)", logPath, err)
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("前の UDS を消せない: %w", err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("egress の UDS で待ち受けられない: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("UDS の権限を設定できない: %w", err)
	}
	audit := newAuditLog(logFile, auditQueue, auditRunBudget)
	p = &hostProxy{
		srv: egress.New(egress.Config{Allow: allow, Audit: audit}), audit: audit, logFile: logFile, logPath: logPath,
		logStart: fi.Size(), lock: lock, sockPath: sock, serveErr: make(chan error, 1),
	}
	go func() { p.serveErr <- p.srv.Serve(l) }()
	return p, nil
}

// Close は、egress を止め (進行中の接続を閉じる)、監査を書き終えて、ロックを離す。捨てた監査の行の数と、待ち受けの異常な終了を返す。
func (p *hostProxy) Close() (dropped int64, serveErr error) {
	p.srv.Close()
	if err := <-p.serveErr; err != nil && !errors.Is(err, egress.ErrClosed) {
		serveErr = err
	}
	dropped = p.audit.Close()
	os.Remove(p.sockPath)
	p.logFile.Close()
	p.lock.Close()
	return dropped, serveErr
}

// checkSockPath は、UDS の path が、上限に収まることを確かめる。
func checkSockPath(sock string) error {
	if n := len(sock); n > maxSockPath {
		return fmt.Errorf("egress の UDS の path が長すぎる (%d バイト。上限 %d): %s\n--state-dir を短い path にする", n, maxSockPath, sock)
	}
	return nil
}

// lockDir は、dir の lock ファイルの排他ロックを、待たずに取る。取れなければ (同じ dir を使う goro run が動いている) error。
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ロックを開けない: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("同じセッション (--login なら、同じエージェントのログイン) を、別の goro run が使っている。終わってから、もう一度実行する")
		}
		return nil, fmt.Errorf("ロックを取れない: %w", err)
	}
	return f, nil
}

// checkAllow は、--allow に書かれた宛先の一覧を、egress が受け付けるか (host:port の形) を、egress 自身に確かめる。
func checkAllow(allow []string) error {
	err := egress.New(egress.Config{Allow: allow, Audit: io.Discard}).Serve(failListener{})
	if errors.Is(err, egress.ErrConfig) {
		return err
	}
	return nil
}

// failListener は、Accept がすぐ失敗する net.Listener (egress の設定の検証だけに使う)。
type failListener struct{}

func (failListener) Accept() (net.Conn, error) { return nil, errors.New("検証だけ") }
func (failListener) Close() error              { return nil }
func (failListener) Addr() net.Addr            { return &net.UnixAddr{Net: "unix"} }

// allowList は、egress に渡す許可の一覧: エージェント p の既定 (egress.ClaudeHosts など) に、--allow で足したものを、重複なく加える。
func allowList(p agentProfile, extra []string) []string {
	out := p.hosts()
	for _, a := range extra {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// deniedTarget は、許可の一覧に無くて拒否された宛先と、その回数。
type deniedTarget struct {
	Target string
	Count  int
}

// maxLogRead は、拒否の一覧を作るために、egress.log から読む量の上限 (バイト)。書く量の上限 (auditRunBudget) より、少し大きい。
const maxLogRead = auditRunBudget + 1<<20

// deniedTargets は、egress.log の start バイト目から後ろ (この起動の分) を読み、許可の一覧に無くて拒否された宛先を、
// 回数の多い順 (同じなら、初めて出た順) に返す。多すぎるときは、上位 max 件と、残りの件数。
func deniedTargets(logPath string, start int64, max int) (top []deniedTarget, more int, err error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	return parseDenied(io.LimitReader(f, maxLogRead), max)
}

// parseDenied は、監査の JSON の行から、deniedTargets の一覧を作る。読めない行と、上限を超えた長い行は、飛ばす。
func parseDenied(r io.Reader, max int) (top []deniedTarget, more int, err error) {
	var order []string
	counts := map[string]int{}
	br := bufio.NewReaderSize(r, 4<<10)
	for {
		line, tooLong, rerr := readLine(br, 4<<10)
		if !tooLong && len(line) > 0 {
			var rec struct{ Event, Reason, Target string }
			if json.Unmarshal(line, &rec) == nil && rec.Event == "deny" && rec.Reason == "not-allowed" && rec.Target != "" {
				if counts[rec.Target] == 0 {
					order = append(order, rec.Target)
				}
				counts[rec.Target]++
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				err = rerr
			}
			break
		}
	}
	for _, t := range order {
		top = append(top, deniedTarget{Target: t, Count: counts[t]})
	}
	slices.SortStableFunc(top, func(a, b deniedTarget) int { return b.Count - a.Count })
	if len(top) > max {
		more = len(top) - max
		top = top[:max]
	}
	return top, more, err
}

// readLine は、br から 1 行 (改行を除く) を読む。limit バイトを超える行は、読み捨てて、tooLong を返す。
func readLine(br *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	for {
		part, isPrefix, err := br.ReadLine()
		if err != nil {
			return line, tooLong, err
		}
		if len(line)+len(part) > limit {
			tooLong = true
		} else if !tooLong {
			line = append(line, part...)
		}
		if !isPrefix {
			return line, tooLong, nil
		}
	}
}

// sanitize は、s (檻が決める文字列を含みうる) を、端末に出しても安全な形 (制御文字・双方向制御・行区切りなどを ? にしたもの) にする。
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (0x80 <= r && r < 0xa0) || r == 0xfffd || r == 0x2028 || r == 0x2029 ||
			(0x200b <= r && r <= 0x200f) || (0x202a <= r && r <= 0x202e) || (0x2060 <= r && r <= 0x206f) || r == 0xfeff {
			return '?'
		}
		return r
	}, s)
}

//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// termDialTimeout は、端末ビューの UDS への 1 回の接続試行に許す時間。
const termDialTimeout = 500 * time.Millisecond

// spawnReadyTimeout は、goronation serve を起動してから、UDS で待ち受け始める (bwrap の起動・clone を含む)
// までに許す時間。
const spawnReadyTimeout = 30 * time.Second

// serveReadyRE は、goronation serve が UDS で待ち受け始めたときに出す行 (serve.go の runServe) から、
// セッション ID を取り出す。
var serveReadyRE = regexp.MustCompile(`^goronation serve: .+ で待ち受けている \(session=(\S+)\)$`)

// dialTermSocket は、group・sessionID の端末ビューの UDS に、待たずに接続を試みる。
func dialTermSocket(stateDir, sessionID string) (net.Conn, error) {
	return net.DialTimeout("unix", termSocketPath(stateDir, defaultGroup, sessionID), termDialTimeout)
}

// spawnServe は、goronation serve --state-dir stateDir <extraArgs...> (--repo PATH か --session ID の
// どちらかを含む) を子プロセスとして起動し、UDS で待ち受け始めるまで (serveReadyRE の行が出るまで)
// 待って、そのセッション ID を返す。
//
// goronation web は、起こした goronation serve の生存を追跡しない (goronation-web-plan §3(a) の決定: 何も永続化しない。
// 次に同じセッションが要るときは、改めて dial すればよい)。子は Setsid で新しいセッションにし、
// goronation web が (端末から Ctrl-C 等で) 終わっても、道連れにしない。
func spawnServe(stateDir, self string, extraArgs []string) (sessionID string, err error) {
	args := append([]string{"serve", "--state-dir", stateDir}, extraArgs...)
	cmd := exec.Command(self, args...)
	cmd.Args[0] = "goronation" // argv[0] は常に "goronation" にする (self の実体の path とは切り離す。ps 等での
	// 見た目の一貫性のためだけで、dispatch は argv[0] を読まない。結合テストが、テストバイナリ自身を
	// self として渡す慣習 (run_bwrap_linux_test.go の runFixture.start と同じ) とも、これで噛み合う)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("goronation serve の起動を準備できない: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("goronation serve を起動できない: %w", err)
	}

	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(stderrPipe)
		var lines []string
		for sc.Scan() {
			line := sc.Text()
			if m := serveReadyRE.FindStringSubmatch(line); m != nil {
				done <- result{id: m[1]}
				io.Copy(io.Discard, stderrPipe) // 残りは読み捨てる (パイプを詰まらせない)
				return
			}
			lines = append(lines, line)
		}
		done <- result{err: fmt.Errorf("起動しなかった: %s", strings.Join(lines, " / "))}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			cmd.Wait() // ゾンビにしない
			return "", res.err
		}
		go cmd.Wait() // 子の終了を回収するだけ (生存の追跡はしない)
		return res.id, nil
	case <-time.After(spawnReadyTimeout):
		cmd.Process.Kill()
		cmd.Wait()
		return "", fmt.Errorf("%s 以内に起動しなかった", spawnReadyTimeout)
	}
}

// ensureTermSocket は、sessionID の端末ビューの UDS への接続を確かめる: すでに goronation serve が動いて
// いれば、すぐ繋がる。動いていなければ、goronation serve --session sessionID を起動してから、もう一度
// 確かめる (起動が「別の goronation serve がすでに使っている」で失敗したときも、同じ理由でもう一度確かめる:
// 競合していた側が、ごく短時間で bind し終えているはず)。
func ensureTermSocket(stateDir, self, sessionID string) error {
	if c, err := dialTermSocket(stateDir, sessionID); err == nil {
		c.Close()
		return nil
	}
	_, spawnErr := spawnServe(stateDir, self, []string{"--session", sessionID})
	if c, err := dialTermSocket(stateDir, sessionID); err == nil {
		c.Close()
		return nil
	}
	if spawnErr != nil {
		return spawnErr
	}
	return fmt.Errorf("起動したはずの goronation serve に繋がらない")
}

// termIDRE は、URL の {id} に許す形 (session.Store の ID の形と同じ)。
var termIDRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

// handleTerminalProxy は、requireSession で保護された、/s/{id}/ws。対応する goronation serve (UDS) が
// 動いていなければ起動し、WebSocket の中身を解釈しない素通しで中継する (httputil.ReverseProxy が、
// Upgrade もハイジャックして中継するので、goronation web 側に WebSocket 固有のコードは要らない)。
func (s *webServer) handleTerminalProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !termIDRE.MatchString(id) {
		http.Error(w, "壊れたセッション ID", http.StatusBadRequest)
		return
	}
	if err := ensureTermSocket(s.stateDir, s.self, id); err != nil {
		http.Error(w, "端末ビューを起動できない: "+err.Error(), http.StatusBadGateway)
		return
	}
	sockPath := termSocketPath(s.stateDir, defaultGroup, id)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "term.sock" // Transport が UDS へ直接 dial するので、実際には使われない
			req.URL.Path = "/"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	proxy.ServeHTTP(w, r)
}

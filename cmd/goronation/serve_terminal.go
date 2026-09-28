//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/nananek/goronation/cmd/internal/termrelay"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// broadcastWriteTimeout は、pty master から読んだ 1 まとまりを、繋がっている viewer へ送るときの上限。
// 応答しない viewer (ブラウザが固まった・ネットワークが詰まったなど) が、この時間を過ぎても書き終わら
// なければ、viewer を外す (readMaster のゴルーチンが、応答しない 1 つの viewer に無期限に足止めされる
// と、pty への書き込みも詰まりかねず、エージェント自身の動作にも影響しうるため)。
const broadcastWriteTimeout = 5 * time.Second

// errSessionEnded は、すでに終わったセッションへの接続を試みたときの error。
var errSessionEnded = errors.New("goronation serve: セッションはすでに終わっている")

// termSession は、goronation serve が起こした、高々 1 つの檻 (エージェント) と、その pty master への
// アクセスをまとめる。生きている間、pty master を読み続けるゴルーチンが 1 つだけ動き、その時点で
// 繋がっている WebSocket の viewer (高々 1 つ。無ければ nil) へ書き込む。誰も見ていない間の出力は、
// 貯めずに捨てる (§4-4 の決定を参照)。
type termSession struct {
	id     string
	ctx    context.Context // startTermSession に渡された ctx (goronation serve の shutdown で取り消される)
	master *os.File
	proxy  *hostProxy
	cage   *bwrap.Cmd

	mu     sync.Mutex
	viewer *termrelay.Conn
	ended  bool

	done chan struct{} // 檻が終わり、後片付け (master・proxy を閉じる) まで済んだら閉じる
	err  error         // 檻の Wait の結果 (nil なら正常終了)
}

// startTermSession は、egress (proxy) を起こし、専用の pty を用意して、cfg (PTY は上書きして true に
// する) の檻を起動する。ctx が取り消されると、檻が終わり (bwrap.Start の契約)、run のゴルーチンが
// 後片付け (proxy.Close・master.Close) まで自動で進める。
func startTermSession(ctx context.Context, id string, cfg cageConfig, allow []string) (*termSession, error) {
	proxy, err := startProxy(cfg.RunDir, allow, nil)
	if err != nil {
		return nil, fmt.Errorf("egress を起動できない: %w", err)
	}
	cfg.PTY = true
	spec := cageSpec(cfg)

	master, slave, err := openHostPty()
	if err != nil {
		proxy.Close()
		return nil, fmt.Errorf("pty を用意できない: %w", err)
	}
	defer slave.Close() // 檻が fork/exec で引き継いだ後は、この複製は要らない
	spec.Stdin, spec.Stdout, spec.Stderr = slave, slave, slave

	cage, err := bwrap.Start(ctx, spec)
	if err != nil {
		master.Close()
		proxy.Close()
		return nil, fmt.Errorf("檻を起動できない: %w", err)
	}

	s := &termSession{id: id, ctx: ctx, master: master, proxy: proxy, cage: cage, done: make(chan struct{})}
	go s.run()
	go s.readMaster()
	return s, nil
}

// run は、檻の終了を待ち、後片付けをして done を閉じる。
func (s *termSession) run() {
	err := s.cage.Wait()
	s.mu.Lock()
	s.ended = true
	v := s.viewer
	s.viewer = nil
	s.err = err
	s.mu.Unlock()
	if v != nil {
		v.Close(termrelay.StatusGoingAway, "セッションが終わった")
	}
	s.master.Close() // readMaster の Read を中断させる (すでに閉じていれば no-op)
	s.proxy.Close()
	close(s.done)
}

// Wait は、檻が終わり、後片付けが済むまで待つ (goronation serve の graceful shutdown が使う)。
func (s *termSession) Wait() error {
	<-s.done
	return s.err
}

// isEnded は、セッションがすでに終わっているか。
func (s *termSession) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// readMaster は、master から読み続け、その都度 broadcast する。真の EOF/error (run が master を
// 閉じた。=檻が終わった) まで戻らない。
func (s *termSession) readMaster() {
	buf := make([]byte, 4096)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			s.broadcast(cp)
		}
		if err != nil {
			return
		}
	}
}

// broadcast は、data を、その時点の viewer (あれば) へ送る。書き終わらなければ (broadcastWriteTimeout
// を参照)、viewer を外して、接続を切る。
func (s *termSession) broadcast(data []byte) {
	s.mu.Lock()
	v := s.viewer
	s.mu.Unlock()
	if v == nil {
		return
	}
	wctx, cancel := context.WithTimeout(s.ctx, broadcastWriteTimeout)
	defer cancel()
	if err := v.WriteBinary(wctx, data); err != nil {
		s.mu.Lock()
		if s.viewer == v {
			s.viewer = nil
		}
		s.mu.Unlock()
		v.CloseNow()
	}
}

// attach は、conn を、このセッションの唯一の viewer にする (それまでの viewer があれば、置き換えて
// 閉じる: 「端末がついてくる」という §4-4 の決定)。s.ctx が取り消されるか、conn 側の入力が終わる
// (ブラウザが切断する) までブロックする。
func (s *termSession) attach(conn *termrelay.Conn) error {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		conn.Close(termrelay.StatusGoingAway, "セッションはすでに終わっている")
		return errSessionEnded
	}
	old := s.viewer
	s.viewer = conn
	s.mu.Unlock()
	if old != nil {
		old.Close(termrelay.StatusNormalClosure, "別の接続に置き換えられた")
	}

	err := conn.ReadLoop(s.ctx, func(data []byte) {
		s.master.Write(data)
	}, func(cols, rows int) {
		setWinsize(s.master, winsize{Row: uint16(rows), Col: uint16(cols)})
	})

	s.mu.Lock()
	if s.viewer == conn {
		s.viewer = nil
	}
	s.mu.Unlock()
	return err
}

// startServeTermSession は、--repo (新しいセッションを作る) か --session (既存のセッションを再開する)
// から、goronation run と同じ組み立てで cageConfig を作り、startTermSession を呼ぶ (§0-3 の決定どおり、
// goronation serve は自分自身が作った・作るセッションだけを扱う)。--push・--allow・--bin は、この PR の範囲外
// (goronation-serve-plan §0-3 の「見送った範囲」)。TERM は、ホストの実端末ではなく xterm.js が話す形式
// (xterm-256color) に固定する (goronation serve 自身の TERM は無関係)。
func startServeTermSession(ctx context.Context, stateDir, sessionID, agentFlag, name, email, repo string, agentArgs []string, stderr io.Writer) (*termSession, error) {
	host, sessStore, agent, existing, agentExe, self, dirs, err := prepareAgentLaunch(stateDir, sessionID, agentFlag, "")
	if err != nil {
		return nil, err
	}
	isNew := repo != ""
	tgt, home, err := resolveRepoTarget(ctx, sessStore, dirs, agent, repo, name, email, existing)
	if err != nil {
		return nil, err
	}
	if isNew {
		fmt.Fprintf(stderr, "goronation serve: セッション %s を作った\n", tgt.id)
	} else {
		fmt.Fprintf(stderr, "goronation serve: セッション %s を再開した\n", tgt.id)
	}
	// managed 設定 (bash/Bash の deny) は、goronation run と同じく、起動のたびに書く (goronation serve の檻も、
	// repo の内容が信頼できないことに変わりはない)。
	managedConfig, err := writeManagedConfig(agent, tgt.runDir)
	if err != nil {
		return nil, fmt.Errorf("managed 設定を書けない: %w", err)
	}
	cfg := cageConfig{
		Host: host, Agent: agent, AgentExe: agentExe, GoroExe: self, CACerts: existingDir("/etc/ssl/certs"),
		RunDir: tgt.runDir, AgentHome: home, AuthDir: dirs.auth, Work: tgt.work, Term: "xterm-256color", TZ: hostTZ(),
		Args: agentArgs, MCPServers: mcpServersFor(), ManagedConfig: managedConfig,
	}
	return startTermSession(ctx, tgt.id, cfg, allowList(agent, nil))
}

// handleTerminal は、端末ビューの WebSocket endpoint。認証は、この関数の責務ではない: goronation serve は
// UDS 専用で、繋いでくるのは goronation web の reverse proxy だけという前提 (UDS に繋げること自体が信頼の
// 境界。goronation-web-plan の決定)。
func handleTerminal(term *termSession, w http.ResponseWriter, r *http.Request) {
	if term.isEnded() {
		http.Error(w, "セッションはすでに終わっている", http.StatusGone)
		return
	}
	conn, err := termrelay.Accept(w, r)
	if err != nil {
		return // termrelay (coder/websocket) が、必要な応答をすでに書いている
	}
	term.attach(conn)
}

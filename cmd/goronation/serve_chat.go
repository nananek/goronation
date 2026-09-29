//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nananek/goronation/cmd/internal/chat"
	"github.com/nananek/goronation/sandbox/bwrap"
)

// chatStderrCap は、エージェントの標準エラー出力を、serve の標準エラー出力に残す上限 (バイト。超えた分は捨てる。UI には出さない)。
const chatStderrCap = 64 << 10

// chatReadDrain は、檻が終わった後に、標準出力の残りを読み切るのを待つ上限。
const chatReadDrain = 5 * time.Second

// chatSession は、goronation serve --chat が起こした、高々 1 つの檻 (エージェント) と、その会話 (chat.Session) をまとめる (termSession の兄弟)。
// pty は使わない: エージェントの標準入出力は、入力用・出力用の別々の socketpair (stdioPair) で、標準エラー出力だけが pipe (上限つきで、
// serve の標準エラー出力に残す)。承認フローの完全性 (ADR 0010 の B3) のため、檻の中の同じ uid のプロセスが、標準入出力の socket に届かない
// ようにする: エージェントは読めない複製から起動して dumpable=0 にし、goronation init は PID 1 で dumpable=0 にする (ADR 0012)。
// 配線 (エージェントの種類・標準形式) は chat の Launch・Session に閉じていて、ここは、名前・引数・バイト列・イベントの JSON だけを扱う。
type chatSession struct {
	id     string
	proxy  *hostProxy
	cage   *bwrap.Cmd
	pair   *stdioPair
	cancel context.CancelFunc
	stderr io.Writer

	// Chat は、会話 (Send・Resolve・Stop) と Hub (Subscribe)。
	Chat *chat.Session

	done chan struct{} // 檻が終わり、会話の終了 (Finish)・後片付けまで済んだら閉じる
	exit int           // 終了コード (done の後に読む)
}

// errChatRoot は、root で chat を動かそうとしたときの error。
var errChatRoot = errors.New("goronation serve --chat は root では動かせない (檻に capability が残り、標準入出力を奪われる。非 root の利用者で動かす)")

// startChatSession は、egress (proxy) を起こし、標準入出力の socketpair を用意して、cfg の檻を起動し、会話を始める。cfg の Agent・Args は、
// launch の引数 + args にし、AgentExe は、読めない複製 (exeDir の下) に置き換え、NonDumpable を立てる。ctx が取り消されると、檻が終わる。
func startChatSession(ctx context.Context, id string, cfg cageConfig, allow []string, launch chat.Launch, args []string, exeDir string, stderr io.Writer) (*chatSession, error) {
	if os.Geteuid() == 0 {
		return nil, errChatRoot
	}
	exe, err := unreadableExeCopy(exeDir, cfg.AgentExe)
	if err != nil {
		return nil, fmt.Errorf("エージェントの読めない複製を用意できない: %w", err)
	}
	cfg.AgentExe = exe
	cfg.NonDumpable = true
	cfg.PTY = false
	cfg.Args = append(launch.Args(), args...)

	proxy, err := startProxy(cfg.RunDir, allow, nil)
	if err != nil {
		return nil, fmt.Errorf("egress を起動できない: %w", err)
	}
	pair, err := newStdioPair()
	if err != nil {
		proxy.Close()
		return nil, err
	}
	spec := cageSpec(cfg)
	spec.NewSession = true // 制御端末を持つ serve から起こしても、檻の中から /dev/tty (運用者の端末) に書けない・読めない
	spec.Stdin, spec.Stdout = pair.AgentIn, pair.AgentOut
	spec.Stderr = &cappedWriter{w: stderr, prefix: "[agent] ", left: chatStderrCap}

	cctx, cancel := context.WithCancel(ctx)
	cage, err := bwrap.Start(cctx, spec)
	if err != nil {
		cancel()
		pair.Close()
		proxy.Close()
		return nil, fmt.Errorf("檻を起動できない: %w", err)
	}
	pair.AgentIn.Close() // 檻に渡した後は、この複製は要らない (閉じないと、エージェントが終わっても EOF にならない)
	pair.AgentOut.Close()

	s := &chatSession{id: id, proxy: proxy, cage: cage, pair: pair, cancel: cancel, stderr: stderr, done: make(chan struct{})}
	s.Chat = chat.NewSession(chat.SessionConfig{
		Launch: launch, ID: id, Input: pair.In,
		// 手動の「終了」: エージェントの入力を閉じ、檻を止める。
		OnStop: func() {
			pair.In.CloseWrite()
			cancel()
		},
	})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		s.Chat.ReadOutput(pair.Out)
	}()
	go s.run(readDone)
	return s, nil
}

// run は、檻の終了を待ち、標準出力の残りを読み切って会話を終え、後片付けをして done を閉じる。
func (s *chatSession) run(readDone <-chan struct{}) {
	err := s.cage.Wait()
	s.exit = exitCodeOf(err, s.stderr)
	select {
	case <-readDone: // 檻の中のプロセスは全部終わったので、EOF まで読み切る
	case <-time.After(chatReadDrain):
	}
	s.Chat.Finish(s.exit)
	s.cancel()
	s.pair.Close()
	s.proxy.Close()
	close(s.done)
}

// Wait は、檻が終わり、後片付けが済むまで待つ。
func (s *chatSession) Wait() int {
	<-s.done
	return s.exit
}

// sanitizeStderr は、p の、\n・\t 以外の制御文字 (C0・DEL・C1)・不正な UTF-8 を ? に置き換える。エージェントの標準エラー出力は敵対入力で、
// 運用者の端末に、タイトルの書き換え・画面の消去などの制御列を、届けさせない。
func sanitizeStderr(p []byte) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			return '?'
		}
		return r
	}, string(p))
}

// cappedWriter は、w に、prefix を付けて、left バイトまで書き、超えた分は捨てる (書いたことにする: エージェントを止めない)。
// prefix は "goronation serve: " で始めない (goronation web が読む ready 行に、エージェントの行が一致して、セッション ID を偽れるため)。
type cappedWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	left   int
	bol    bool // 行頭か (prefix を付ける位置)
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if len(p) > c.left {
		p = p[:c.left]
	}
	c.left -= len(p)
	p = []byte(sanitizeStderr(p))
	for len(p) > 0 && c.w != nil {
		if !c.bol {
			io.WriteString(c.w, c.prefix)
			c.bol = true
		}
		i := 0
		for i < len(p) && p[i] != '\n' {
			i++
		}
		if i < len(p) {
			i++
			c.bol = false
		}
		c.w.Write(p[:i])
		p = p[i:]
	}
	return n, nil
}

// startServeChatSession は、--repo (新しいセッションを作る) か --session (既存のセッションを再開する) から、goronation run と同じ組み立てで
// cageConfig を作り、startChatSession を呼ぶ (startServeTermSession の兄弟)。TERM は dumb (端末は無い)。
func startServeChatSession(ctx context.Context, stateDir, sessionID, agentFlag, name, email, repo string, agentArgs []string, stderr io.Writer) (*chatSession, error) {
	host, sessStore, agent, existing, agentExe, self, dirs, err := prepareAgentLaunch(stateDir, sessionID, agentFlag, "")
	if err != nil {
		return nil, err
	}
	launch, err := chat.Agent(agent.name)
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
	cfg := cageConfig{
		Host: host, Agent: agent, AgentExe: agentExe, GoroExe: self, CACerts: existingDir("/etc/ssl/certs"),
		RunDir: tgt.runDir, AgentHome: home, AuthDir: dirs.auth, Work: tgt.work, Term: "dumb", TZ: hostTZ(),
	}
	return startChatSession(ctx, tgt.id, cfg, allowList(agent, nil), launch, agentArgs, filepath.Join(filepath.Dir(dirs.auth), "exe"), stderr)
}

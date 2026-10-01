package chat

import (
	"fmt"
	"io"

	"github.com/nananek/goronation/agent/claude"
	"github.com/nananek/goronation/agent/opencode"
	"github.com/nananek/goronation/core/agent"
)

// Launch は、エージェント 1 種類の、chat セッションでの起動の組 (不透明な型)。cmd/goronation は、名前 (Agent) から Launch を得て、
// 引数 (Args) を檻に渡し、標準入出力を Session に繋ぐだけで、エージェントの種類も、標準形式の型も知らない。
type Launch struct {
	name      string
	args      []string
	newStream func() agent.Stream
	transport Transport
}

// Transport は、エージェントの標準入出力の使い道 (cmd/goronation が、檻の標準入出力をどう繋ぐかを決める)。
type Transport int

const (
	// TransportStdio は、標準入力に命令の行を書き、標準出力から出力の行を読む (claude の stream-json)。
	TransportStdio Transport = iota
	// TransportHTTP は、標準入力が control (HTTP の中継)・標準出力が起動の状態の 1 行で、行は SSE から読み、命令は HTTP で送る (opencode)。
	TransportHTTP
)

// Agent は、エージェント名 (--agent の値) の Launch を返す。claude (標準入出力) と opencode (HTTP + SSE)。未知の名前は error。
func Agent(name string) (Launch, error) {
	switch name {
	case claude.Name:
		return Launch{
			name: name,
			// PR⓪ の採取で決まった組 (ADR 0010)。--permission-prompt-tool stdio は、隠しフラグ。initialize は、アダプタが、最初の prompt の
			// raw の先頭に含める (起動直後に別に送らない)。
			// --permission-mode default を明示する: claude 2.1.285 は、既定の権限モードが auto (分類器が許可を決める) に変わり、
			// --permission-prompt-tool stdio を付けても can_use_tool の要求が出ず、tool が、人間の承認なしで実行された (2.1.284 は default。PR⑧ の実物の確認で発見)。
			// --setting-sources user は、repo の .claude/settings.local.json の allow・.claude/settings.json の hooks (どちらも承認なしで tool を実行させる) を切る (ADR 0017)。HOME の設定と、managed settings (host の root だけが書ける) は切れない。
			args:      []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-prompt-tool", "stdio", "--permission-mode", "default", "--setting-sources", "user"},
			newStream: claude.Adapter{}.NewStream,
			transport: TransportStdio,
		}, nil
	case opencode.Name:
		return Launch{
			name: name,
			// serve --stdio: 標準入力が control・標準出力が init の状態の 1 行 (ADR 0028・0030)。--port は、実行時に決まるので、呼び手が足す。
			// --hostname は 127.0.0.1 に固定する (呼び手の引数では上書きさせない。ADR 0052)。
			args:      []string{"serve", "--stdio", "--hostname", "127.0.0.1"},
			newStream: opencode.Adapter{}.NewStream,
			transport: TransportHTTP,
		}, nil
	}
	return Launch{}, fmt.Errorf("chat: --chat で動かせないエージェント %q (対応は claude か opencode)", name)
}

// Name は、エージェント名。
func (l Launch) Name() string { return l.name }

// Transport は、標準入出力の使い道。
func (l Launch) Transport() Transport { return l.transport }

// Args は、檻の中でエージェントに渡す引数 (呼び手の引数は、この後ろ)。
func (l Launch) Args() []string { return append([]string(nil), l.args...) }

// SessionConfig は、Session の部品。
type SessionConfig struct {
	Launch Launch
	// ID は、Event の session (serve のセッション ID)。
	ID string
	// Input は、エージェントの入力 (標準入力)。Session が、有界のキュー越しに書く。
	Input io.Writer
	// OnStop は、Stop の最初の 1 回だけ (または書き込みの失敗のとき) 呼ぶ (エージェントの入力を閉じ、檻を止める)。nil でもよい。
	OnStop func()
	// Hub は、リングバッファの設定 (ゼロ値なら既定)。
	Hub HubConfig
	// Store・OnDegrade は、耐久イベントログ (HubConfig の同名の項目に渡す。nil なら、リングだけ。ADR 0053)。
	Store     EventStore
	OnDegrade func(reason string)
	// OptionalContentHash が true なら、応答の content_hash が無くても通す。ゼロ値 (false) は必須 (ADR 0042 決定 3)。本番の呼び手は立てない (試験だけ)。
	OptionalContentHash bool
}

// Session は、1 回のエージェントの起動の、読む側 (Hub) と書く側 (Conversation) をつなぐ。プロセス・ネットワークには触れない。
//
//	s := chat.NewSession(cfg)
//	go func() { s.ReadOutput(agentStdout); s.Finish(exitCode) }() // エージェントが終わるまで、出力を読み、終わったら Finish
//	s.Conv.Send(text); s.Conv.ResolveIn(s.Conv.Generation(), id, outcome); s.Conv.Stop(); s.Hub.Subscribe()
type Session struct {
	Conv *Conversation
	Hub  *Hub

	q *QueuedWriter
}

// NewSession は、Session を作る (エージェントの起動の前に。Input へは、Send・Resolve から書く)。
func NewSession(cfg SessionConfig) *Session {
	if cfg.Store != nil {
		cfg.Hub.Store, cfg.Hub.OnDegrade = cfg.Store, cfg.OnDegrade
	}
	s := &Session{Hub: NewHub(cfg.Hub)}
	s.Conv = NewConversation(ConversationConfig{
		Feed:               NewFeed(cfg.Launch.newStream(), cfg.ID, nil),
		Hub:                s.Hub,
		Write:              func(line []byte) error { return s.q.WriteLine(line) },
		OnStop:             func() { s.q.Discard(); s.stop(cfg) },
		RequireContentHash: !cfg.OptionalContentHash,
	})
	s.q = NewQueuedWriter(cfg.Input, 0, 0, func(error) { s.Conv.Stop() }) // 入力に書けない (エージェントが終わった・壊れた): 会話を終える
	return s
}

func (s *Session) stop(cfg SessionConfig) {
	if cfg.OnStop != nil {
		cfg.OnStop()
	}
}

// ReadOutput は、エージェントの標準出力 r を、EOF か読み込みの error まで、1 行ずつ会話に流す。行の上限超過・不正な行・変換できない行は、
// 捨てて続ける。標準出力を読み切ったら戻る (呼び手が、エージェントの終了を待って、Finish を呼ぶ)。
func (s *Session) ReadOutput(r io.Reader) {
	lr := NewLineReader(r, 0)
	for {
		line, err := lr.Next()
		switch err {
		case nil:
			s.Conv.OnLine(line) // 変換できない行は、何も配られずに、error が返る。捨てる
		case ErrLineTooLong, ErrBadLine:
		default:
			return
		}
	}
}

// Finish は、エージェントの終了 (exit code) を記録する: 未決を全部失効させ、Hub を終了にし、書き込みのキューを閉じる。何度呼んでもよい。
func (s *Session) Finish(exit int) {
	s.Conv.Close(exit)
	s.q.Discard()
}

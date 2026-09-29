package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/nananek/goronation/core/agent"
	v0 "github.com/nananek/goronation/spec/v0"
)

// Event は、UI・API に出す 1 つのイベント。JSON は v0.UIEnvelope (Raw を含まない) の、改行を含まない 1 行。
// Envelope (Raw を含む) は、この package の外に出さない (tools/archtest の v0-only-in-chat)。
type Event struct {
	Seq     uint64
	Type    string
	Durable bool
	JSON    []byte
}

// size は、Hub のバイト上限の勘定に使う大きさ (JSON の長さと、Event 自体の分)。
func (e Event) size() int { return len(e.JSON) + 64 }

// DefaultMaxEvent は、Event の JSON の大きさの上限 (バイト)。行の上限の 2 倍 (リングバッファの既定の半分)。これを超える
// Event は作らずに、ErrEventTooLarge を返す (エージェントの 1 行が、リングの履歴を押し出す・購読者を外すのを防ぐ)。
const DefaultMaxEvent = 2 * DefaultMaxLine

// ErrEventTooLarge は、Event の JSON が DefaultMaxEvent を超えた (その Event は捨てた。seq は進めない)。
var ErrEventTooLarge = errors.New("chat: イベントが大きすぎる (捨てた)")

// Feed は、エージェントの Stream (agent.Stream) を包み、出てくる Envelope に ID・TS・Session・Seq を振って、
// Public() の JSON にする。Raw は、ここで落ちる (Event は Raw を持たない)。Decode・Encode は Mutex で直列になる (標準出力の読み取りと、ユーザーの操作の経路が、別の goroutine から呼んでも、seq が重複しない)。
// ただし Stream (agent.Stream) 自体も、この Mutex の中でしか呼ばない。
//
// seq は 0 から、イベントごとに 1 ずつ (durable でないものにも)。id は "e<seq>"。ts は RFC 3339 (UTC・ミリ秒)。
// Stream が返す V・ID・TS・Session・Seq は、値を問わず上書きする (エージェントの出力がこれらを決められない)。
type Feed struct {
	stream  agent.Stream
	session string
	now     func() time.Time
	mu      sync.Mutex
	seq     uint64
}

// NewFeed は、Feed を作る。now が nil なら time.Now。
func NewFeed(stream agent.Stream, session string, now func() time.Time) *Feed {
	if now == nil {
		now = time.Now
	}
	return &Feed{stream: stream, session: session, now: now}
}

// Decode は、エージェントの出力の 1 行 (LineReader の返す行) を、Event の列にする。Stream の error は、そのまま返す
// (seq は進めない)。
func (f *Feed) Decode(line []byte) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	envs, err := f.stream.DecodeFrame(line)
	if err != nil {
		return nil, err
	}
	return f.events(envs)
}

// Encode は、Command を、エージェントの入力に書く 1 行にし、合成されたイベント (turn.started など) を Event にする。
// Stream の error は、そのまま返す (seq は進めない)。
func (f *Feed) Encode(cmd v0.Command) (raw []byte, events []Event, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, envs, err := f.stream.EncodeCommand(cmd)
	if err != nil {
		return nil, nil, err
	}
	events, err = f.events(envs)
	if err != nil {
		return nil, nil, err
	}
	return raw, events, nil
}

func (f *Feed) events(envs []v0.Envelope) ([]Event, error) {
	out := make([]Event, 0, len(envs))
	ts := f.now().UTC().Format("2006-01-02T15:04:05.000Z")
	seq := f.seq
	for _, e := range envs {
		e.V, e.ID, e.TS, e.Session, e.Seq = v0.Version, "e"+strconv.FormatUint(seq, 10), ts, f.session, seq
		if len(e.Data) == 0 {
			e.Data = json.RawMessage(`{}`)
		}
		b, err := marshalLine(e.Public())
		if err != nil {
			return nil, err
		}
		out = append(out, Event{Seq: seq, Type: e.Type, Durable: e.Durable, JSON: b})
		seq++
	}
	f.seq = seq
	return out, nil
}

// marshalLine は、v を改行を含まない 1 行の JSON にする。RawMessage は圧縮される (生の改行は JSON の文字列に入れられない)。
// < > & は、エスケープしない (6 倍に膨らみ、上限以内の 1 行がリングと購読者の上限を超えるため)。JSON は SSE の data: と
// JSON.parse・textContent だけに使い、HTML には埋め込まない。U+2028・U+2029 は SSE の行の区切りではない。
func marshalLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) > DefaultMaxEvent {
		return nil, ErrEventTooLarge
	}
	return b, nil
}

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

// DefaultMaxEvent は、Event の JSON の大きさの上限 (バイト)。< > & はエスケープしない (6 倍に膨らみ、上限以内の 1 行が購読者を外し、
// 履歴を押し出せたため)。JSON は SSE の data: と JSON.parse だけに使い、HTML には埋め込まない (これは archtest では検査できない)。
// 行の上限の 2 倍 (リングバッファの既定の半分)。これを超える
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

// EncodeQuiet は、Encode と同じだが、Stream が合成するイベント (permission.resolved の by=human など) を捨てる (seq を進めない)。
// 状態機械が、自分で理由 (by=policy など) を付けた Event を Emit するときに使う。
func (f *Feed) EncodeQuiet(cmd v0.Command) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _, err := f.stream.EncodeCommand(cmd)
	return raw, err
}

// Emit は、エージェントのフレームを介さない Event (会話の状態機械が作る、権限要求の失効など) を、Data (JSON にできる値) から作る。
func (f *Feed) Emit(typ string, durable bool, data any) (Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := marshalLine(data)
	if err != nil {
		return Event{}, err
	}
	evs, err := f.events([]v0.Envelope{{V: v0.Version, Type: typ, Durable: durable, Data: b}})
	if err != nil {
		return Event{}, err
	}
	return evs[0], nil
}

func (f *Feed) events(envs []v0.Envelope) ([]Event, error) {
	out := make([]Event, 0, len(envs))
	ts := f.now().UTC().Format("2006-01-02T15:04:05.000Z")
	seq := f.seq
	for _, e := range envs {
		e = sealRequest(e)
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

// sealRequest は、permission.requested・form.requested を、共通の層の規則に通す (ADR 0042・0047): data を v0 の型に読み、Validate を通らなければ、
// agent.frame (data 空。承認も回答もできない形) にする。通れば、content_hash を、この層が計算した値で入れる (アダプタが付けた値は、上書きする)。
// 型に無い欄は、ここで落ちる (アダプタの出す data は、語彙の欄だけが UI・API に出る)。それ以外の type は、そのまま。
func sealRequest(e v0.Envelope) v0.Envelope {
	var data any
	switch e.Type {
	case v0.TypePermissionRequested:
		var p v0.PermissionRequested
		if json.Unmarshal(e.Data, &p) != nil || p.Validate() != nil {
			return asFrame(e)
		}
		p.ContentHash = p.Hash()
		data = p
	case v0.TypeFormRequested:
		var f v0.FormRequested
		if json.Unmarshal(e.Data, &f) != nil || f.Validate() != nil {
			return asFrame(e)
		}
		f.ContentHash = f.Hash()
		data = f
	default:
		return e
	}
	b, err := marshalLine(data)
	if err != nil { // 大きすぎる (DefaultMaxEvent を超える data)。承認できない形にして落とす (Event の上限の検査は、封筒を書いた後にもある)
		return asFrame(e)
	}
	e.Data = b
	return e
}

// asFrame は、e を、対応する語彙が無いフレーム (agent.frame。data 空・durable でない) にする。
func asFrame(e v0.Envelope) v0.Envelope {
	e.Type, e.Durable, e.Data = v0.TypeAgentFrame, false, json.RawMessage(`{}`)
	return e
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

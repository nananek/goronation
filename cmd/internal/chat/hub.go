package chat

import (
	"context"
	"errors"
	"io"
	"sync"
)

// Hub の既定の上限。
const (
	DefaultHubMaxBytes      = 4 << 20 // リングバッファ (溢れたら古い方から捨てる)
	DefaultSubMaxBytes      = 4 << 20 // 購読者ごとのキュー (読むのが遅くて溢れたら、その購読者を外す)
	DefaultSubMaxEvents     = 4096    // 同 (件数)
	hubSubscribersHardLimit = 1 << 16 // 購読者の数の、最後の砦 (通常の上限は呼び手 (web の接続数の上限) が持つ)
)

// ErrSlowSubscriber は、購読者のキューが上限を超えたので、Hub が外した (Subscription.Next が返す)。読み直すには、
// 新しく Subscribe する (バッファに残っている分は、また全部届く)。
var ErrSlowSubscriber = errors.New("chat: 購読者が遅いので外した")

// ErrTooManySubscribers は、購読者の数が最後の砦を超えた。
var ErrTooManySubscribers = errors.New("chat: 購読者が多すぎる")

// HubConfig は、Hub の上限。0 以下の項目は既定の値。
type HubConfig struct {
	MaxBytes  int // リングバッファの上限 (Event の JSON の長さの合計 + 1 件あたりの固定の分)
	SubBytes  int // 購読者ごとのキューの上限 (バイト)
	SubEvents int // 購読者ごとのキューの上限 (件数)
}

// Hub は、イベントのメモリ上のリングバッファと、購読者への配信。耐久ストアではない (M2 で置き換える。ADR 0009)。
// 並行に使える。Publish が購読者のキューに積むだけで待たないので、遅い購読者が Publish (エージェントの出力の読み取り) を止めない。
type Hub struct {
	cfg HubConfig

	mu       sync.Mutex
	ring     []Event // ring[head:] が有効。head までは捨てた分 (Event{} に潰してある)
	head     int
	bytes    int
	nextSeq  uint64 // 次に受ける Event の Seq (連続していなくても、最後の Seq + 1 まで進める)
	subs     map[*Subscription]struct{}
	ended    bool
	exitCode int
}

// NewHub は、Hub を作る。
func NewHub(cfg HubConfig) *Hub {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultHubMaxBytes
	}
	if cfg.SubBytes <= 0 {
		cfg.SubBytes = DefaultSubMaxBytes
	}
	if cfg.SubEvents <= 0 {
		cfg.SubEvents = DefaultSubMaxEvents
	}
	return &Hub{cfg: cfg, subs: map[*Subscription]struct{}{}}
}

// Publish は、events をバッファに足し、購読者に配る。終了後は何もしない。上限を超えたら、古い Event から捨てる
// (直近の 1 件は、それだけで上限を超えても残す)。キューが溢れた購読者は外す。
func (h *Hub) Publish(events ...Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return
	}
	for _, e := range events {
		h.ring = append(h.ring, e)
		h.bytes += e.size()
		h.nextSeq = e.Seq + 1
		for s := range h.subs {
			s.push(e, h.cfg)
		}
	}
	for h.head < len(h.ring)-1 && h.bytes > h.cfg.MaxBytes {
		h.bytes -= h.ring[h.head].size()
		h.ring[h.head] = Event{} // 捨てた Event の JSON を、スライスの裏に残さない
		h.head++
	}
	// 捨てるたびに全体を写すと、小さな行を大量に出すエージェントで 1 件ごとに O(バッファ) になる。捨てた分が半分を超えたときだけ詰める。
	if h.head > 0 && h.head >= len(h.ring)/2 {
		h.ring = append([]Event(nil), h.ring[h.head:]...)
		h.head = 0
	}
	for s := range h.subs {
		if s.isDone() {
			delete(h.subs, s)
		}
	}
}

// Close は、エージェントの終了 (exit code つき) を記録し、購読者に終わりを知らせる。2 回目以降は何もしない。
// 終了後の Subscribe は、バッファの中身を全部返したあと、終わりを返す。
func (h *Hub) Close(exit int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return
	}
	h.ended, h.exitCode = true, exit
	for s := range h.subs {
		s.finish(nil)
	}
	h.subs = map[*Subscription]struct{}{}
}

// Subscription は、Subscribe の時点のバッファの中身 (Snapshot) と、そのあとのライブのイベントの受け口。
// Snapshot とライブの境目に、取りこぼしも重複も無い (同じ Mutex の中で、バッファの写しと登録をする)。
type Subscription struct {
	hub *Hub

	// Snapshot は、Subscribe の時点でバッファにあった Event (Seq 順)。呼び手が書き換えてはいけない。
	Snapshot []Event
	// FirstSeq は、Snapshot の最初の Event の Seq (空なら、次に来る Event の Seq)。0 でなければ、それより前の分は溢れて捨てた
	// (seq は 0 から)。UI が「古い分は省略」を出すのに使う (SSE の hello の first_seq)。
	FirstSeq uint64

	mu     sync.Mutex
	queue  []Event
	bytes  int
	done   bool
	err    error // done のときの理由。nil は正常な終了 (Hub.Close)
	exit   int
	notify chan struct{}
}

// Subscribe は、購読を始める。使い終わったら Close を呼ぶ。
func (h *Hub) Subscribe() (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= hubSubscribersHardLimit {
		return nil, ErrTooManySubscribers
	}
	s := &Subscription{hub: h, Snapshot: append([]Event(nil), h.ring[h.head:]...), FirstSeq: h.nextSeq, notify: make(chan struct{}, 1)}
	if len(s.Snapshot) > 0 {
		s.FirstSeq = s.Snapshot[0].Seq
	}
	if h.ended {
		s.done, s.exit = true, h.exitCode
		return s, nil
	}
	h.subs[s] = struct{}{}
	return s, nil
}

func (s *Subscription) push(e Event, cfg HubConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	// キューが空なら、1 件は必ず受ける (1 件だけで上限を超える大きなイベントで、追いついている購読者を外さない。リングの「直近の 1 件は残す」と同じ)。
	if len(s.queue) > 0 && (len(s.queue)+1 > cfg.SubEvents || s.bytes+e.size() > cfg.SubBytes) {
		s.queue, s.bytes = nil, 0
		s.done, s.err = true, ErrSlowSubscriber
		s.wake()
		return
	}
	s.queue = append(s.queue, e)
	s.bytes += e.size()
	s.wake()
}

// finish は、Hub.Close (err が nil) で呼ぶ。キューに残っている分は、Next が先に返す。
func (s *Subscription) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done, s.err, s.exit = true, err, s.hub.exitCode
	s.wake()
}

func (s *Subscription) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Subscription) isDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Next は、次のライブのイベントを返す。なければ、届く・終わる・ctx が終わるまで待つ。
// Hub.Close で終わったら io.EOF (このとき Exit が有効)、遅くて外されたら ErrSlowSubscriber、ctx が終わったら ctx.Err()。
// キューに残っているイベントは、終わりや外された後でも、先に返す (ただし、外されたときはキューを捨てているので残らない)。
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			e := s.queue[0]
			s.queue[0] = Event{}
			s.queue = s.queue[1:]
			s.bytes -= e.size()
			if len(s.queue) == 0 {
				s.queue = nil
			}
			s.mu.Unlock()
			return e, nil
		}
		if s.done {
			err := s.err
			s.mu.Unlock()
			if err == nil {
				return Event{}, io.EOF
			}
			return Event{}, err
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// Exit は、Hub.Close で終わった (Next が io.EOF を返した) ときの、エージェントの exit code。
func (s *Subscription) Exit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}

// Close は、購読をやめる (Hub から外す)。何度呼んでもよい。
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	delete(s.hub.subs, s)
	s.hub.mu.Unlock()
	s.mu.Lock()
	if !s.done {
		s.done, s.err = true, context.Canceled
	}
	s.queue, s.bytes = nil, 0
	s.mu.Unlock()
}

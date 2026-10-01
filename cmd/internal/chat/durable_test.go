package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore は、遅延・失敗・停止・記録を注入できる偽の EventStore (メモリ上。eventlog の *Store の契約を、chat の試験のために写したもの)。
type fakeStore struct {
	mu       sync.Mutex
	rows     map[uint64][]byte
	pinned   map[uint64]bool
	ops      []StoreOp
	trimmed  []int64
	firstUnp uint64

	applyDelay time.Duration
	applyErr   error
	gate       chan struct{} // 非 nil なら、Apply はこれが閉じるまで待つ
	rangeErr   error
	rangeRows  func(after, before uint64, limit int) []StoredEvent // 非 nil なら、Range の返す行を差し替える (改ざんの注入)
	applied    atomic.Int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[uint64][]byte{}, pinned: map[uint64]bool{}}
}

func (f *fakeStore) Apply(op StoreOp) error {
	if f.gate != nil {
		<-f.gate
	}
	time.Sleep(f.applyDelay)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.ops = append(f.ops, op)
	for _, e := range op.Append {
		f.rows[e.Seq] = e.Payload
	}
	for _, s := range op.Pin {
		if _, ok := f.rows[s]; ok {
			f.pinned[s] = true
		}
	}
	for _, s := range op.Unpin {
		delete(f.pinned, s)
	}
	f.applied.Add(1)
	return nil
}

func (f *fakeStore) Range(after, before uint64, limit int) ([]StoredEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rangeErr != nil {
		return nil, f.rangeErr
	}
	if f.rangeRows != nil {
		return f.rangeRows(after, before, limit), nil
	}
	var seqs []uint64
	for s := range f.rows {
		if s > after && s < before {
			seqs = append(seqs, s)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	if len(seqs) > limit {
		seqs = seqs[:limit]
	}
	out := make([]StoredEvent, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, StoredEvent{Seq: s, Payload: f.rows[s]})
	}
	return out, nil
}

func (f *fakeStore) FirstUnpinned() (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firstUnp, f.firstUnp != 0
}

func (f *fakeStore) Trim(budget int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trimmed = append(f.trimmed, budget)
	return f.firstUnp, nil
}

func (f *fakeStore) snapshotOps() []StoreOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StoreOp(nil), f.ops...)
}

// dev は、Store に入れてよい形の Event (JSON に seq・type・durable がある)。
func dev(seq uint64) Event {
	return Event{Seq: seq, Type: "message.text", Durable: true, JSON: []byte(fmt.Sprintf(`{"seq":%d,"type":"message.text","durable":true,"data":{"t":"x"}}`, seq))}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("待ちきれない: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// fill は、seq from..to-1 の durable Event を、1 件ずつ書き込み済みになるのを待って publish する (小さなリングで、書き手が追いつく)。
func fill(t *testing.T, h *Hub, from, to uint64) {
	t.Helper()
	for i := from; i < to; i++ {
		h.Publish(dev(i))
		waitFor(t, "書き込み", func() bool { return h.w.written.Load() >= int64(i) })
	}
}

// degradeProbe は、OnDegrade の呼ばれた回数と理由を数える。
type degradeProbe struct {
	n      atomic.Int32
	mu     sync.Mutex
	reason string
}

func (p *degradeProbe) fn(reason string) {
	p.n.Add(1)
	p.mu.Lock()
	p.reason = reason
	p.mu.Unlock()
}
func (p *degradeProbe) get() string { p.mu.Lock(); defer p.mu.Unlock(); return p.reason }

// drain は、sub の Backfill → Snapshot を読み尽くして、seq の列を返す。
func drain(t *testing.T, s *Subscription) []uint64 {
	t.Helper()
	var seqs []uint64
	for {
		evs, err := s.Backfill.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			seqs = append(seqs, e.Seq)
		}
	}
	for _, e := range s.Snapshot {
		seqs = append(seqs, e.Seq)
	}
	return seqs
}

func contiguous(seqs []uint64, from uint64) error {
	for i, s := range seqs {
		if s != from+uint64(i) {
			return fmt.Errorf("位置 %d: seq %d (期待 %d) 列=%v", i, s, from+uint64(i), seqs)
		}
	}
	return nil
}

// リングから溢れた分は DB から、そのあとはリング・ライブから、欠けず重複せず続く。
func TestSubscribeAfterStoreRingLive(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 5 * 150})
	for i := uint64(0); i < 40; i++ {
		h.Publish(dev(i))
		waitFor(t, "書き込み", func() bool { return h.w.written.Load() >= int64(i) })
	}
	s, err := h.SubscribeAfter(9)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Resumed() || !s.Durable() {
		t.Fatalf("resumed=%v durable=%v", s.Resumed(), s.Durable())
	}
	if s.Backfill == nil || len(s.Snapshot) == 0 || s.Snapshot[0].Seq <= 10 {
		t.Fatalf("リングが小さいので Backfill が要る: snapshot=%v", s.Snapshot)
	}
	seqs := drain(t, s)
	if err := contiguous(seqs, 10); err != nil || seqs[len(seqs)-1] != 39 {
		t.Fatalf("%v", err)
	}
	h.Publish(dev(40))
	e, err := s.Next(context.Background())
	if err != nil || e.Seq != 40 {
		t.Fatalf("live %v %v", e, err)
	}
}

// 書き込み・押し出し・購読を、並行にランダムに混ぜても、Resumed の購読は after+1 から欠けず重複せず続く。
func TestSubscribeAfterNoGapNoDuplicateConcurrent(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 64 * 150, SubBytes: 1 << 30, SubEvents: 1 << 20})
	const n = 3000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(0); i < n; i++ {
			h.Publish(dev(i))
			if i%3 == 0 {
				time.Sleep(20 * time.Microsecond) // 書き手が追いつく余地
			}
		}
		h.Close(0)
	}()
	var resumed, plain atomic.Int32
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for k := 0; k < 40; k++ {
				time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
				h.mu.Lock()
				next := h.nextSeq
				h.mu.Unlock()
				if next == 0 {
					continue
				}
				after := uint64(rng.Int63n(int64(next)))
				s, err := h.SubscribeAfter(after)
				if err != nil {
					t.Errorf("after=%d: %v", after, err)
					return
				}
				var seqs []uint64
				for {
					evs, err := s.Backfill.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Errorf("backfill: %v", err)
						return
					}
					for _, e := range evs {
						seqs = append(seqs, e.Seq)
					}
				}
				for _, e := range s.Snapshot {
					seqs = append(seqs, e.Seq)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				for {
					e, err := s.Next(ctx)
					if err != nil {
						break
					}
					seqs = append(seqs, e.Seq)
				}
				cancel()
				s.Close()
				if s.Resumed() {
					resumed.Add(1)
					if len(seqs) == 0 || seqs[0] != after+1 {
						t.Errorf("after=%d 先頭=%v", after, seqs[:min(3, len(seqs))])
						return
					}
					if err := contiguous(seqs, after+1); err != nil || seqs[len(seqs)-1] != n-1 {
						t.Errorf("after=%d: %v 末尾=%d", after, err, seqs[len(seqs)-1])
						return
					}
				} else {
					plain.Add(1)
					if err := contiguous(seqs, seqs[0]); err != nil {
						t.Errorf("全再送: %v", err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	t.Logf("resumed=%d 全再送=%d", resumed.Load(), plain.Load())
	if resumed.Load() == 0 {
		t.Fatal("resumed の購読が 1 つも無い (試験が意味を持たない)")
	}
}

// Store が止まっていても (Apply が返らない)、Hub.Update と Subscribe は待たない (ADR 0027 完了条件 2)。
func TestUpdateNeverWaitsForStore(t *testing.T) {
	st := newFakeStore()
	st.gate = make(chan struct{})
	defer close(st.gate)
	h := NewHub(HubConfig{Store: st})
	var worst time.Duration
	for i := uint64(0); i < 2000; i++ {
		start := time.Now()
		h.Publish(dev(i))
		worst = max(worst, time.Since(start))
	}
	s, err := h.Subscribe()
	if err != nil || len(s.Snapshot) != 2000 {
		t.Fatalf("snapshot=%d err=%v", len(s.Snapshot), err)
	}
	if worst > 50*time.Millisecond {
		t.Fatalf("Publish が %v かかった (Store の停止を待っている)", worst)
	}
}

// Store が 100 ms 遅くても、Update の所要時間 (最大) は変わらない。
func TestUpdateLatencyIndependentOfStoreDelay(t *testing.T) {
	measure := func(delay time.Duration) time.Duration {
		st := newFakeStore()
		st.applyDelay = delay
		h := NewHub(HubConfig{Store: st, slowCommit: time.Hour, hardCommit: time.Hour})
		var worst time.Duration
		for i := uint64(0); i < 500; i++ {
			start := time.Now()
			h.Publish(dev(i))
			worst = max(worst, time.Since(start))
			time.Sleep(100 * time.Microsecond)
		}
		return worst
	}
	base, slow := measure(0), measure(100*time.Millisecond)
	t.Logf("最大: 遅延なし %v・100 ms %v", base, slow)
	if slow > base+20*time.Millisecond {
		t.Fatalf("Store の遅延が Update に及んだ: %v → %v", base, slow)
	}
}

func assertDegraded(t *testing.T, h *Hub, p *degradeProbe, wantReason string) {
	t.Helper()
	waitFor(t, "OnDegrade", func() bool { return p.n.Load() == 1 })
	if !strings.Contains(p.get(), wantReason) {
		t.Fatalf("理由=%q (期待 %q)", p.get(), wantReason)
	}
	if h.Durable() {
		t.Fatal("縮退したのに Durable")
	}
	h.Publish(dev(1 << 20)) // 縮退しても、配信は続く
	s, err := h.SubscribeAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Resumed() || s.Durable() || s.Backfill != nil {
		t.Fatalf("縮退中は全再送: resumed=%v durable=%v", s.Resumed(), s.Durable())
	}
	time.Sleep(20 * time.Millisecond)
	if p.n.Load() != 1 {
		t.Fatalf("OnDegrade が %d 回", p.n.Load())
	}
}

func TestDegradeOnApplyError(t *testing.T) {
	st := newFakeStore()
	st.applyErr = errors.New("disk full")
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn})
	h.Publish(dev(0), dev(1))
	assertDegraded(t, h, p, "disk full")
}

func TestDegradeOnQueueOverflow(t *testing.T) {
	st := newFakeStore()
	st.gate = make(chan struct{})
	defer close(st.gate)
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, StoreQueueBytes: 2000})
	for i := uint64(0); i < 100; i++ {
		h.Publish(dev(i))
	}
	assertDegraded(t, h, p, "キューが溢れた")
}

func TestDegradeWhenRingEvictsUnwritten(t *testing.T) {
	st := newFakeStore()
	st.gate = make(chan struct{})
	defer close(st.gate)
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, MaxBytes: 3 * 150})
	for i := uint64(0); i < 10; i++ {
		h.Publish(dev(i))
	}
	assertDegraded(t, h, p, "押し出し")
}

func TestNoDegradeWhenEvictingNonDurable(t *testing.T) {
	st := newFakeStore()
	st.gate = make(chan struct{})
	defer close(st.gate)
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, MaxBytes: 3 * 150})
	for i := uint64(0); i < 50; i++ {
		h.Publish(Event{Seq: i, Type: "message.delta", JSON: []byte(`{"seq":0}`)}) // durable でない: 書かない・押し出しても縮退しない
	}
	time.Sleep(20 * time.Millisecond)
	if p.n.Load() != 0 || !h.Durable() {
		t.Fatal("durable でない Event の押し出しで縮退した")
	}
}

func TestDegradeOnHardSlowCommit(t *testing.T) {
	st := newFakeStore()
	st.applyDelay = 40 * time.Millisecond
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, hardCommit: 20 * time.Millisecond, slowCommit: time.Hour})
	h.Publish(dev(0))
	assertDegraded(t, h, p, "かかった")
}

func TestDegradeOnSlowRun(t *testing.T) {
	st := newFakeStore()
	st.applyDelay = 8 * time.Millisecond
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, slowCommit: 4 * time.Millisecond, hardCommit: time.Hour, slowRun: 3})
	for i := uint64(0); i < 3; i++ {
		h.Publish(dev(i))
		waitFor(t, "書き込み", func() bool { return st.applied.Load() >= int64(i)+1 || p.n.Load() > 0 })
	}
	assertDegraded(t, h, p, "続いた")
}

// 遅い書き込みが連続しなければ、縮退しない。
func TestSlowCommitsThatAreNotConsecutiveDoNotDegrade(t *testing.T) {
	st := newFakeStore()
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, slowCommit: 4 * time.Millisecond, hardCommit: time.Hour, slowRun: 3})
	for i := uint64(0); i < 12; i++ {
		if i%3 != 2 {
			st.mu.Lock()
			st.applyDelay = 8 * time.Millisecond
			st.mu.Unlock()
		} else {
			st.mu.Lock()
			st.applyDelay = 0
			st.mu.Unlock()
		}
		h.Publish(dev(i))
		waitFor(t, "書き込み", func() bool { return st.applied.Load() >= int64(i)+1 })
	}
	if p.n.Load() != 0 {
		t.Fatalf("縮退した: %s", p.get())
	}
}

// Hub が拒否した pin は Store に出さない。固定していない seq の unpin も出さない。
func TestStoreMirrorsHubPinDecisions(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxPinned: 2})
	evs := []Event{dev(0), dev(1), dev(2), dev(3)}
	h.Update(evs, []Event{evs[0], evs[1], evs[2]}, []uint64{3, 99}) // 3 つ目は上限で拒否。3・99 は固定していない
	h.Update(nil, nil, []uint64{0})                                 // 固定していた seq の解除
	h.Close(0)
	if err := h.WaitStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.pinned[1] || st.pinned[0] || st.pinned[2] || st.pinned[3] || len(st.pinned) != 1 {
		t.Fatalf("pinned=%v", st.pinned)
	}
	for _, op := range st.ops {
		for _, s := range append(append([]uint64(nil), op.Pin...), op.Unpin...) {
			if s == 2 || s == 3 || s == 99 {
				t.Fatalf("Hub が決めていない操作を出した: %+v", op)
			}
		}
	}
}

func TestMergeOpsKeepsLastPinState(t *testing.T) {
	got := mergeOps([]StoreOp{
		{Append: []StoredEvent{{Seq: 1}}, Pin: []uint64{1}},
		{Append: []StoredEvent{{Seq: 2}}, Pin: []uint64{2}, Unpin: []uint64{1}},
		{Pin: []uint64{1}},
	})
	if len(got.Append) != 2 || fmt.Sprint(got.Pin) != "[1 2]" || len(got.Unpin) != 0 {
		t.Fatalf("%+v", got)
	}
	got = mergeOps([]StoreOp{{Pin: []uint64{5}}, {Unpin: []uint64{5}}})
	if len(got.Pin) != 0 || fmt.Sprint(got.Unpin) != "[5]" {
		t.Fatalf("%+v", got)
	}
	one := normalizePins(StoreOp{Pin: []uint64{7, 8}, Unpin: []uint64{7}}) // 1 回の Update の中で pin → unpin: 解除が勝つ
	if fmt.Sprint(one.Pin) != "[8]" {
		t.Fatalf("%+v", one)
	}
}

// 固定した Event は、DB から (リングより前でも) 届く。
func TestPinnedReachesResumeBeyondRing(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150})
	first := dev(0)
	h.Update([]Event{first}, []Event{first}, nil)
	fill(t, h, 1, 30)
	s, err := h.SubscribeAfter(0)
	if err != nil || !s.Resumed() {
		t.Fatalf("resumed=%v err=%v", s != nil && s.Resumed(), err)
	}
	if err := contiguous(drain(t, s), 1); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.pinned[0] {
		t.Fatal("固定の写しが Store に無い")
	}
}

// durable でない固定の Event が範囲にある (DB に無い) ときは、全再送に倒す。
func TestNonDurablePinnedFallsBackToFullReplay(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150})
	fill(t, h, 0, 1)
	nd := Event{Seq: 1, Type: "x", JSON: []byte(`{"seq":1}`)}
	h.Update([]Event{nd}, []Event{nd}, nil)
	fill(t, h, 2, 30)
	s, err := h.SubscribeAfter(0)
	if err != nil || s.Resumed() {
		t.Fatalf("resumed=%v err=%v", s != nil && s.Resumed(), err)
	}
	if len(s.Snapshot) == 0 || s.Snapshot[0].Seq != 1 {
		t.Fatalf("固定の Event を、全再送の先頭に含める: %v", s.Snapshot)
	}
}

func TestSubscribeAfterBadAndEdges(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st})
	if _, err := h.SubscribeAfter(0); !errors.Is(err, ErrBadAfter) {
		t.Fatalf("空の Hub: %v", err)
	}
	h.Publish(dev(0), dev(1), dev(2))
	for _, a := range []uint64{3, 4, 1 << 62, ^uint64(0)} {
		if _, err := h.SubscribeAfter(a); !errors.Is(err, ErrBadAfter) {
			t.Fatalf("after=%d: %v", a, err)
		}
	}
	s, err := h.SubscribeAfter(2) // 最後の seq: 続きは無い
	if err != nil || !s.Resumed() || s.Backfill != nil || len(s.Snapshot) != 0 {
		t.Fatalf("%v %+v", err, s)
	}
	s.Close()
	// nil の Backfill は、すぐ EOF
	if _, err := (*Backfill)(nil).Next(); err != io.EOF {
		t.Fatal(err)
	}
}

// Store が無い Hub は、今までどおり (SubscribeAfter は全再送)。
func TestSubscribeAfterWithoutStore(t *testing.T) {
	h := NewHub(HubConfig{})
	h.Publish(dev(0), dev(1))
	s, err := h.SubscribeAfter(0)
	if err != nil || s.Resumed() || s.Durable() || len(s.Snapshot) != 2 {
		t.Fatalf("%v %+v", err, s)
	}
	if h.Durable() || h.WaitStore(context.Background()) != nil {
		t.Fatal("Store 無しの Durable・WaitStore")
	}
}

// 終了した Hub でも、DB の続き → リング → 終わり、で再接続できる (S6)。キューに残った分は、Close のあとに書き切る。
func TestSubscribeAfterOnEndedHub(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150})
	fill(t, h, 0, 30)
	h.Close(3)
	if err := h.WaitStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := h.SubscribeAfter(5)
	if err != nil || !s.Resumed() {
		t.Fatalf("%v", err)
	}
	if err := contiguous(drain(t, s), 6); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(context.Background()); err != io.EOF || s.Exit() != 3 {
		t.Fatalf("終わり: %v exit=%d", err, s.Exit())
	}
}

// Close のあとの WaitStore は ctx で打ち切れる (Store が止まっていても)。
func TestWaitStoreHonorsContext(t *testing.T) {
	st := newFakeStore()
	st.gate = make(chan struct{})
	defer close(st.gate)
	h := NewHub(HubConfig{Store: st})
	h.Publish(dev(0))
	h.Close(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := h.WaitStore(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

// 遅い購読者が Backfill の最中にライブのキューで外されても、Backfill は壊れず、進んだ after で繋ぎ直せる。
func TestSlowSubscriberDroppedDuringBackfill(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150, SubEvents: 3})
	fill(t, h, 0, 40)
	s, err := h.SubscribeAfter(0)
	if err != nil || !s.Resumed() {
		t.Fatal(err)
	}
	fill(t, h, 40, 50) // Backfill を読む前に、ライブが溢れる
	evs, err := s.Backfill.Next()
	if err != nil || len(evs) == 0 {
		t.Fatalf("%v", err)
	}
	if _, err := s.Next(context.Background()); !errors.Is(err, ErrSlowSubscriber) {
		t.Fatalf("外されるはず: %v", err)
	}
	last := evs[len(evs)-1].Seq
	s2, err := h.SubscribeAfter(last) // 進んだ after で繋ぎ直す
	if err != nil || !s2.Resumed() {
		t.Fatal(err)
	}
	seqs := drain(t, s2)
	if err := contiguous(seqs, last+1); err != nil || seqs[len(seqs)-1] != 49 {
		t.Fatal(err)
	}
}

// Store が返した行を信用しない: 改ざんされた行は、配らずに error・縮退。
func TestBackfillRejectsTamperedRows(t *testing.T) {
	good := func(s uint64) StoredEvent { return StoredEvent{Seq: s, Payload: dev(s).JSON} }
	cases := map[string]func(after, before uint64, limit int) []StoredEvent{
		"seq が範囲外": func(a, b uint64, l int) []StoredEvent { return []StoredEvent{good(b + 1)} },
		"seq が戻る":  func(a, b uint64, l int) []StoredEvent { return []StoredEvent{good(a + 1), good(a + 1)} },
		"件数の超過":    func(a, b uint64, l int) []StoredEvent { return []StoredEvent{good(a + 1), good(a + 2), good(a + 3)} },
		"JSON でない": func(a, b uint64, l int) []StoredEvent {
			return []StoredEvent{{Seq: a + 1, Payload: []byte(`not json`)}}
		},
		"JSON の seq 違い": func(a, b uint64, l int) []StoredEvent { return []StoredEvent{{Seq: a + 1, Payload: dev(a + 2).JSON}} },
		"改行を含む": func(a, b uint64, l int) []StoredEvent {
			return []StoredEvent{{Seq: a + 1, Payload: []byte(fmt.Sprintf("{\"seq\":%d,\n\"type\":\"x\",\"durable\":true}", a+1))}}
		},
		"type に改行": func(a, b uint64, l int) []StoredEvent {
			return []StoredEvent{{Seq: a + 1, Payload: []byte(fmt.Sprintf(`{"seq":%d,"type":"x\ny","durable":true}`, a+1))}}
		},
		"durable でない": func(a, b uint64, l int) []StoredEvent {
			return []StoredEvent{{Seq: a + 1, Payload: []byte(fmt.Sprintf(`{"seq":%d,"type":"x","durable":false}`, a+1))}}
		},
		"valid で大きすぎる": func(a, b uint64, l int) []StoredEvent {
			return []StoredEvent{{Seq: a + 1, Payload: []byte(fmt.Sprintf(`{"seq":%d,"type":"x","durable":true,"p":"%s"}`, a+1, strings.Repeat("a", DefaultMaxEvent)))}}
		},
		"空": func(a, b uint64, l int) []StoredEvent { return []StoredEvent{{Seq: a + 1}} },
		"大きすぎる": func(a, b uint64, l int) []StoredEvent {
			return []StoredEvent{{Seq: a + 1, Payload: make([]byte, DefaultMaxEvent+1)}}
		},
	}
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			st := newFakeStore()
			p := &degradeProbe{}
			h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, MaxBytes: 4 * 150})
			fill(t, h, 0, 30)
			s, err := h.SubscribeAfter(0)
			if err != nil || !s.Resumed() {
				t.Fatal(err)
			}
			st.mu.Lock()
			st.rangeRows = rows
			st.mu.Unlock()
			s.Backfill.batch = 2
			if evs, err := s.Backfill.Next(); !errors.Is(err, ErrBadStoredEvent) || len(evs) != 0 {
				t.Fatalf("evs=%d err=%v", len(evs), err)
			}
			waitFor(t, "縮退", func() bool { return p.n.Load() == 1 })
			if _, err := s.Backfill.Next(); err != io.EOF {
				t.Fatalf("失敗のあとは EOF: %v", err)
			}
		})
	}
}

func TestBackfillStoreErrorDegrades(t *testing.T) {
	st := newFakeStore()
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, MaxBytes: 4 * 150})
	fill(t, h, 0, 30)
	s, _ := h.SubscribeAfter(0)
	st.mu.Lock()
	st.rangeErr = errors.New("open /home/u/.local/state/goronation/events.db: db closed")
	st.mu.Unlock()
	_, err := s.Backfill.Next()
	if !errors.Is(err, ErrBackfillRead) || strings.Contains(err.Error(), "/home/") {
		t.Fatalf("固定の error を返す (Store の error の path を呼び手に返さない): %v", err)
	}
	waitFor(t, "縮退", func() bool { return p.n.Load() == 1 })
	if s2, _ := h.SubscribeAfter(0); s2.Resumed() {
		t.Fatal("縮退のあとは全再送")
	}
}

// 上限を超えたら Trim (目標は上限の 3/4)。Trim のあと、first_seq は DB の先頭になる。
func TestGCTrimsAndFirstSeqFollowsStore(t *testing.T) {
	st := newFakeStore()
	st.firstUnp = 7
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150, StoreMaxBytes: 1000})
	for i := uint64(0); i < 40; i++ {
		h.Publish(dev(i))
		waitFor(t, "書き込み", func() bool { return h.w.written.Load() >= int64(i) })
	}
	st.mu.Lock()
	n, budget := len(st.trimmed), int64(0)
	if n > 0 {
		budget = st.trimmed[0]
	}
	st.mu.Unlock()
	if n == 0 || budget != 750 {
		t.Fatalf("Trim %d 回・目標 %d", n, budget)
	}
	if n > 12 {
		t.Fatalf("Trim が多すぎる (%d 回。書き込みごとに走っている)", n)
	}
	st.mu.Lock()
	for s := range st.rows { // 削られた分を、Store の側で反映する
		if s < 7 {
			delete(st.rows, s)
		}
	}
	st.mu.Unlock()
	s, err := h.SubscribeAfter(2)
	if err != nil || !s.Resumed() {
		t.Fatal(err)
	}
	if s.FirstSeq != 7 {
		t.Fatalf("FirstSeq=%d (Trim のあとは、DB の先頭)", s.FirstSeq)
	}
	s2, _ := h.SubscribeAfter(8)
	if s2.FirstSeq != 7 {
		t.Fatalf("FirstSeq=%d", s2.FirstSeq)
	}
}

// 削っていなければ、省略は無い (first_seq は 0)。
func TestFirstSeqIsZeroWhenNothingTrimmed(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150})
	fill(t, h, 0, 30)
	s, _ := h.SubscribeAfter(3)
	if s.FirstSeq != 0 {
		t.Fatalf("FirstSeq=%d", s.FirstSeq)
	}
}

// Session に Store を渡すと Hub に届く。
func TestSessionPassesStore(t *testing.T) {
	l, err := Agent("claude")
	if err != nil {
		t.Fatal(err)
	}
	st := newFakeStore()
	s := NewSession(SessionConfig{Launch: l, ID: "s", Input: io.Discard, Store: st})
	if s.Hub.w == nil || s.Hub.w.store != st {
		t.Fatal("Store が Hub に渡っていない")
	}
	s.Finish(0)
	if err := s.Hub.WaitStore(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func FuzzSubscribeAfter(f *testing.F) {
	for _, a := range []uint64{0, 1, 5, 29, 30, 31, 1 << 53, ^uint64(0)} {
		f.Add(a)
	}
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150})
	for i := uint64(0); i < 30; i++ {
		h.Publish(dev(i))
	}
	f.Fuzz(func(t *testing.T, after uint64) {
		s, err := h.SubscribeAfter(after)
		if after >= 30 {
			if !errors.Is(err, ErrBadAfter) {
				t.Fatalf("after=%d: %v", after, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if s.Resumed() {
			if err := contiguous(drain(t, s), after+1); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// Backfill を読んでいる最中に GC (Trim) が走ったら、ErrBackfillStale (縮退ではない)。繋ぎ直せば、新しい first_seq が出る。
func TestBackfillStaleWhenTrimRunsDuringBackfill(t *testing.T) {
	st := newFakeStore()
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, MaxBytes: 4 * 150})
	fill(t, h, 0, 30)
	s, err := h.SubscribeAfter(0)
	if err != nil || !s.Resumed() {
		t.Fatal(err)
	}
	s.Backfill.batch = 2
	if _, err := s.Backfill.Next(); err != nil {
		t.Fatal(err)
	}
	h.w.trims.Add(2) // Trim が 1 回走った
	if _, err := s.Backfill.Next(); !errors.Is(err, ErrBackfillStale) {
		t.Fatalf("%v", err)
	}
	if !h.Durable() || p.n.Load() != 0 {
		t.Fatal("GC で縮退してはいけない")
	}
	st.firstUnp = 12
	s2, err := h.SubscribeAfter(0)
	if err != nil || !s2.Resumed() || s2.FirstSeq != 12 {
		t.Fatalf("繋ぎ直し: %v first=%d", err, s2.FirstSeq)
	}
}

// Trim の最中 (奇数) は、first_seq を決められないので全再送。固定でない行が無ければ、first_seq はリングの先頭。
func TestSubscribeAfterDuringTrimAndWithoutUnpinnedRows(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st, MaxBytes: 4 * 150})
	fill(t, h, 0, 30)
	h.w.trims.Add(1)
	if s, _ := h.SubscribeAfter(0); s.Resumed() {
		t.Fatal("Trim の最中は全再送")
	}
	h.w.trims.Add(1)
	s, _ := h.SubscribeAfter(0)
	if !s.Resumed() || s.FirstSeq != s.Snapshot[0].Seq { // FirstUnpinned が無い: リングの先頭
		t.Fatalf("resumed=%v first=%d", s.Resumed(), s.FirstSeq)
	}
}

// 固定の操作だけの洪水も、キューの上限で縮退する (キューが無限に伸びない)。
func TestPinOnlyFloodIsBounded(t *testing.T) {
	st := newFakeStore()
	st.gate = make(chan struct{})
	defer close(st.gate)
	p := &degradeProbe{}
	h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, StoreQueueBytes: 4096})
	e := dev(0)
	h.Update([]Event{e}, []Event{e}, nil)
	for i := 0; i < 1000; i++ {
		h.Update(nil, []Event{e}, nil) // 同じ Event の固定の繰り返し
	}
	waitFor(t, "縮退", func() bool { return p.n.Load() == 1 })
}

// durable でない Event は、Store に書かない。
func TestNonDurableEventsAreNotStored(t *testing.T) {
	st := newFakeStore()
	h := NewHub(HubConfig{Store: st})
	h.Publish(dev(0), Event{Seq: 1, Type: "message.delta", JSON: []byte(`{"seq":1}`)}, dev(2))
	h.Close(0)
	if err := h.WaitStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.rows[1]; ok || len(st.rows) != 2 {
		t.Fatalf("rows=%v", st.rows)
	}
}

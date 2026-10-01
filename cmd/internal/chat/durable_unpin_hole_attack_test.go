package chat

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// 実際の Trim の意味 (固定でない行を古い方から削る。固定の行は残る) を持つ Store。
type trimStore struct {
	mu     sync.Mutex
	rows   map[uint64][]byte
	pinned map[uint64]bool
	trims  int
}

func newTrimStore() *trimStore {
	return &trimStore{rows: map[uint64][]byte{}, pinned: map[uint64]bool{}}
}

func (m *trimStore) Apply(op StoreOp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range op.Append {
		m.rows[e.Seq] = e.Payload
	}
	for _, s := range op.Pin {
		if _, ok := m.rows[s]; ok {
			m.pinned[s] = true
		}
	}
	for _, s := range op.Unpin {
		delete(m.pinned, s)
	}
	return nil
}

func (m *trimStore) Range(after, before uint64, limit int) ([]StoredEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var seqs []uint64
	for s := range m.rows {
		if s > after && s < before {
			seqs = append(seqs, s)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	if len(seqs) > limit {
		seqs = seqs[:limit]
	}
	var out []StoredEvent
	for _, s := range seqs {
		out = append(out, StoredEvent{Seq: s, Payload: m.rows[s]})
	}
	return out, nil
}

// Trim は、固定でない行を古い方から削り、残った固定でない行の最小の seq を返す (無ければ 0)。
func (m *trimStore) Trim(budget int64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trims++
	var seqs []uint64
	var total int64
	for s, p := range m.rows {
		total += int64(len(p))
		if !m.pinned[s] {
			seqs = append(seqs, s)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for len(seqs) > 0 && total > budget {
		total -= int64(len(m.rows[seqs[0]]))
		delete(m.rows, seqs[0])
		seqs = seqs[1:]
	}
	if len(seqs) == 0 {
		return 0, nil
	}
	return seqs[0], nil
}

func (m *trimStore) state() (trims int, has0, has1 bool, pinned int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, has0 = m.rows[0]
	_, has1 = m.rows[1]
	return m.trims, has0, has1, len(m.pinned)
}

// 固定した古い行 (未決の要求) が、GC を免れて残り、あとで解決 (固定を解く) されても、hello の first_seq は、GC が削った範囲を示す
// (Trim が返した、残った固定でない行の最小の seq から決める。固定を解いた古い行には引きずられない)。ADR 0024 決定 2・ADR 0025 完了条件 1。
func TestSubscribeAfterFlagsOmissionWhenOldPinnedRowIsUnpinned(t *testing.T) {
	st := newTrimStore()
	size := dev(0).size()
	h := NewHub(HubConfig{Store: st, MaxBytes: 6 * size, StoreMaxBytes: int64(8 * len(dev(0).JSON))})
	e0 := dev(0)
	h.Update([]Event{e0}, []Event{e0}, nil) // seq 0: 未決の要求 (固定)
	for i := uint64(1); i < 80; i++ {
		h.Publish(dev(i))
		waitFor(t, "書き込み", func() bool { return h.w.written.Load() >= int64(i) })
	}
	waitFor(t, "GC で seq 1 が削られ、固定の seq 0 が残る", func() bool {
		trims, has0, has1, _ := st.state()
		return trims > 0 && has0 && !has1
	})
	h.Update(nil, nil, []uint64{0}) // 解決: 固定を解く (seq 0 は固定でない行になる)
	waitFor(t, "固定の解除の書き込み", func() bool { _, _, _, p := st.state(); return p == 0 })
	time.Sleep(20 * time.Millisecond)
	const after = 5 // 削られた範囲 (seq 1.. は GC で消えた)
	s, err := h.SubscribeAfter(after)
	if err != nil || !s.Resumed() {
		t.Fatalf("SubscribeAfter: %v resumed=%v", err, s != nil && s.Resumed())
	}
	var got []uint64
	for s.Backfill != nil {
		evs, err := s.Backfill.Next()
		if err != nil {
			break
		}
		for _, e := range evs {
			got = append(got, e.Seq)
		}
	}
	missing := true
	for _, q := range got {
		if q == after+1 {
			missing = false
		}
	}
	for _, e := range s.Snapshot {
		if e.Seq == after+1 {
			missing = false
		}
	}
	if missing && s.FirstSeq <= after+1 {
		t.Errorf("seq %d は届かないのに、first_seq=%d は、省略 (first_seq > after+1) を示さない (届いた Backfill=%v)", after+1, s.FirstSeq, got)
	}
}

package chat

import (
	"testing"
	"time"
)

// W (書き込み済みの最大の seq) の初期値は -1 (まだ何も書いていない)。リングから、W を超える durable の Event を押し出すと縮退する (ADR 0027 決定 3)。
// 境界: W=-1 で seq 0 (durable) を押し出す → 縮退する。non-durable なら縮退しない。seq 0 が書き込み済み (W=0) なら縮退しない。
// W=-1 の初期値を 0 にする・比較を >= や +1 にずらす変異で、押し出された未書き込みの durable が、DB にもリングにも無くなる (または、不要に縮退する)。
func TestRingEvictionBoundaryOfWrittenSeq(t *testing.T) {
	size := dev(0).size()
	run := func(gate, waitWritten, seq0Durable bool) (degraded bool) {
		st := newFakeStore()
		if gate {
			st.gate = make(chan struct{})
			defer close(st.gate)
		}
		p := &degradeProbe{}
		h := NewHub(HubConfig{Store: st, OnDegrade: p.fn, MaxBytes: 2*size + size/2}) // 2 件分: seq 2 を足すと seq 0 が押し出される
		e0 := dev(0)
		e0.Durable = seq0Durable
		h.Publish(e0)
		if waitWritten {
			waitFor(t, "seq 0 の書き込み", func() bool { return h.w.written.Load() >= 0 })
		}
		h.Publish(dev(1))
		h.Publish(dev(2))
		time.Sleep(100 * time.Millisecond)
		return p.n.Load() > 0
	}
	if !run(true, false, true) {
		t.Error("W=-1・seq 0 (durable) を押し出したのに、縮退しない (未書き込みの履歴が、DB からもリングからも消える)")
	}
	if run(true, false, false) {
		t.Error("seq 0 が non-durable なのに、縮退した")
	}
	if run(false, true, true) {
		t.Error("seq 0 は書き込み済み (W>=0) なのに、縮退した")
	}
}

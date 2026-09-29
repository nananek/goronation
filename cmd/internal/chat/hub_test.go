package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

func ev(seq uint64, payload string) Event {
	return Event{Seq: seq, Type: "x", Durable: true, JSON: []byte(fmt.Sprintf(`{"seq":%d,"p":%q}`, seq, payload))}
}

func TestHubEvictsOldestByBytes(t *testing.T) {
	h := NewHub(HubConfig{MaxBytes: 3 * (64 + 20)})
	for i := uint64(0); i < 10; i++ {
		h.Publish(Event{Seq: i, JSON: make([]byte, 20)})
	}
	s, err := h.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot) != 3 || s.Snapshot[0].Seq != 7 || s.FirstSeq != 7 || s.Snapshot[2].Seq != 9 {
		t.Fatalf("snapshot=%v first=%d", s.Snapshot, s.FirstSeq)
	}
}

func TestHubKeepsNewestEvenIfOverLimit(t *testing.T) {
	h := NewHub(HubConfig{MaxBytes: 100})
	h.Publish(Event{Seq: 0, JSON: make([]byte, 10)}, Event{Seq: 1, JSON: make([]byte, 5000)})
	s, _ := h.Subscribe()
	if len(s.Snapshot) != 1 || s.Snapshot[0].Seq != 1 {
		t.Fatalf("snapshot=%v", s.Snapshot)
	}
}

func TestHubEmptyFirstSeqIsNext(t *testing.T) {
	h := NewHub(HubConfig{})
	s, _ := h.Subscribe()
	if len(s.Snapshot) != 0 || s.FirstSeq != 0 {
		t.Fatalf("first=%d", s.FirstSeq)
	}
	h.Publish(ev(0, "a"), ev(1, "b"))
	e, err := s.Next(context.Background())
	if err != nil || e.Seq != 0 {
		t.Fatalf("e=%v err=%v", e, err)
	}
}

// Subscribe の最中に Publish が続いても、Snapshot + ライブに、取りこぼしも重複も無い。
func TestHubSnapshotToLiveHasNoGapOrDuplicate(t *testing.T) {
	h := NewHub(HubConfig{MaxBytes: 1 << 30, SubBytes: 1 << 30, SubEvents: 1 << 20})
	const n = 20000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(0); i < n; i++ {
			h.Publish(ev(i, "p"))
		}
		h.Close(7)
	}()
	var subs sync.WaitGroup
	for k := 0; k < 8; k++ {
		subs.Add(1)
		go func() {
			defer subs.Done()
			time.Sleep(time.Duration(k) * 50 * time.Microsecond)
			s, err := h.Subscribe()
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			var want uint64
			if len(s.Snapshot) > 0 {
				want = s.Snapshot[0].Seq
			}
			for _, e := range s.Snapshot {
				if e.Seq != want {
					t.Errorf("snapshot に穴: %d want %d", e.Seq, want)
					return
				}
				want++
			}
			for {
				e, err := s.Next(context.Background())
				if err == io.EOF {
					if want != n || s.Exit() != 7 {
						t.Errorf("終わりが早い・exit=%d: want=%d", s.Exit(), want)
					}
					return
				}
				if err != nil {
					t.Errorf("err=%v", err)
					return
				}
				if e.Seq != want {
					t.Errorf("ライブに穴か重複: %d want %d", e.Seq, want)
					return
				}
				want++
			}
		}()
	}
	wg.Wait()
	subs.Wait()
}

func TestHubSlowSubscriberIsDroppedAndDoesNotBlock(t *testing.T) {
	h := NewHub(HubConfig{SubEvents: 4})
	slow, _ := h.Subscribe()
	fast, _ := h.Subscribe()
	done := make(chan struct{})
	go func() {
		for i := uint64(0); i < 100; i++ {
			h.Publish(ev(i, "p"))
			for { // fast は、遅れずに読む
				if e, err := fast.Next(context.Background()); err != nil || e.Seq == i {
					break
				}
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("遅い購読者が Publish を止めた")
	}
	if _, err := slow.Next(context.Background()); !errors.Is(err, ErrSlowSubscriber) {
		t.Fatalf("err=%v", err)
	}
	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("外した購読者が残っている: %d", n)
	}
	// 読み直せば、バッファの分がまた全部届く。
	again, _ := h.Subscribe()
	if len(again.Snapshot) != 100 {
		t.Fatalf("snapshot=%d", len(again.Snapshot))
	}
}

func TestHubCloseAndLateSubscribe(t *testing.T) {
	h := NewHub(HubConfig{})
	s, _ := h.Subscribe()
	h.Publish(ev(0, "a"))
	h.Close(3)
	h.Publish(ev(1, "after-close")) // 終了後は捨てる
	h.Close(9)                      // 2 回目は無視
	if e, err := s.Next(context.Background()); err != nil || e.Seq != 0 {
		t.Fatalf("e=%v err=%v", e, err)
	}
	if _, err := s.Next(context.Background()); err != io.EOF || s.Exit() != 3 {
		t.Fatalf("err=%v exit=%d", err, s.Exit())
	}
	late, err := h.Subscribe()
	if err != nil || len(late.Snapshot) != 1 {
		t.Fatalf("late=%v err=%v", late, err)
	}
	if _, err := late.Next(context.Background()); err != io.EOF || late.Exit() != 3 {
		t.Fatalf("err=%v exit=%d", err, late.Exit())
	}
}

func TestSubscriptionNextHonorsContextAndClose(t *testing.T) {
	h := NewHub(HubConfig{})
	s, _ := h.Subscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	s.Close()
	s.Close()
	if _, err := s.Next(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("Close した購読者が残っている: %d", n)
	}
	h.Publish(ev(0, "x")) // 閉じた購読者に積まない
}

// 購読者ごとのキューの上限は、件数だけでなくバイトでも効く。
func TestHubSubscriberByteLimit(t *testing.T) {
	h := NewHub(HubConfig{SubBytes: 1000})
	s, _ := h.Subscribe()
	// 1 件だけで上限を超えても、キューが空なら受ける (追いついている購読者を外さない)。
	h.Publish(Event{Seq: 0, JSON: make([]byte, 2000)})
	if e, err := s.Next(context.Background()); err != nil || e.Seq != 0 {
		t.Fatalf("e=%v err=%v", e.Seq, err)
	}
	// 読まないうちに次が積まれて上限を超えたら外す。
	h.Publish(Event{Seq: 1, JSON: make([]byte, 600)}, Event{Seq: 2, JSON: make([]byte, 600)})
	if _, err := s.Next(context.Background()); !errors.Is(err, ErrSlowSubscriber) {
		t.Fatalf("err=%v", err)
	}
}

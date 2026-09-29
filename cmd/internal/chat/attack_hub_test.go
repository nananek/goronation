package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/agent/claude"
)

// リングが満杯でも、1 回の Publish のコストは、リングの大きさによらない (小さなイベントの洪水で、
// Publish の中でリング全体を写し続けて、読み取りと、Mutex を待つ Subscribe・Close を遅くしない)。
func TestHubPublishCostDoesNotGrowWithRingSize(t *testing.T) {
	perPublish := func(maxBytes int) time.Duration {
		h := NewHub(HubConfig{MaxBytes: maxBytes})
		e := Event{Type: "x", Durable: true, JSON: []byte(`{"a":1}`)}
		fill := maxBytes/e.size() + 10
		for i := 0; i < fill; i++ {
			e.Seq = uint64(i)
			h.Publish(e)
		}
		const n = 5000
		start := time.Now()
		for i := 0; i < n; i++ {
			e.Seq = uint64(fill + i)
			h.Publish(e)
		}
		return time.Since(start) / n
	}
	small, large := perPublish(64<<10), perPublish(1<<20)
	t.Logf("1 回の Publish: リング約 900 件 %v・約 14000 件 %v", small, large)
	if large > 6*small+20*time.Microsecond { // 定数時間なら約 1 倍、リング全体の写しなら約 16 倍以上
		t.Errorf("リングが 16 倍大きいと、1 回の Publish が %.1f 倍かかった (リングの大きさに比例)", float64(large)/float64(small))
	}
}

func maxLenLine() string {
	return `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"` + strings.Repeat("<", DefaultMaxLine-200) + `"}]}}`
}

// エージェントの 1 行 (DefaultMaxLine 以内) が、JSON にすると数倍の大きさ (< > & のエスケープ) になっても、
// 既定の設定の購読者を外さない。
func TestOneMaxLengthLineDoesNotEvictSubscribers(t *testing.T) {
	line := maxLenLine()
	if len(line) > DefaultMaxLine {
		t.Fatalf("テストの行が上限を超えている: %d", len(line))
	}
	f := NewFeed(claude.Adapter{}.NewStream(), "S", nil)
	evs, err := f.Decode([]byte(line))
	if err != nil || len(evs) == 0 {
		t.Fatalf("decode: %v %d", err, len(evs))
	}
	h := NewHub(HubConfig{})
	s, err := h.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	h.Publish(evs...)
	if _, err := s.Next(context.Background()); errors.Is(err, ErrSlowSubscriber) {
		t.Errorf("上限以内の 1 行 (JSON %d バイト) で、既定の設定の購読者が外れた", len(evs[0].JSON))
	}
}

// 上限以内の 1 行が、それまでの履歴 (リングの中身) を、全部押し出さない。
func TestOneMaxLengthLineDoesNotEraseHistory(t *testing.T) {
	f := NewFeed(claude.Adapter{}.NewStream(), "S", nil)
	h := NewHub(HubConfig{})
	first, err := f.Decode([]byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"tu","input":{"command":"ls"}}}`))
	if err != nil || len(first) != 1 {
		t.Fatalf("decode: %v %d", err, len(first))
	}
	h.Publish(first...)
	big, err := f.Decode([]byte(maxLenLine()))
	if err != nil {
		t.Fatal(err)
	}
	h.Publish(big...)
	s, _ := h.Subscribe()
	for _, e := range s.Snapshot {
		if e.Seq == first[0].Seq {
			return
		}
	}
	t.Errorf("上限以内の 1 行で、それまでの permission.requested (seq %d) が、リングから消えた (残っているのは seq %d から)", first[0].Seq, s.FirstSeq)
}

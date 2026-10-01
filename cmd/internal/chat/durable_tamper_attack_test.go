package chat

import (
	"errors"
	"fmt"
	"testing"
)

// Store から読んだ行の検査 (storedEvent) は、キー名の大文字小文字を区別する (UI の JS の JSON.parse と同じ読み方)。
// `{"seq":99,"Seq":N,…}` は、構造体への読み (区別しない) では Seq=N で通り、UI には seq=99 として届く。大文字小文字違いのキーを持つ行は拒否する。
func TestBackfillRejectsCaseVariantKeys(t *testing.T) {
	for name, payload := range map[string]func(s uint64) string{
		"seq": func(s uint64) string { return fmt.Sprintf(`{"seq":99,"Seq":%d,"type":"x","durable":true}`, s) },
		"type": func(s uint64) string {
			return fmt.Sprintf(`{"seq":%d,"type":"permission.requested","Type":"x","durable":true}`, s)
		},
		"durable": func(s uint64) string { return fmt.Sprintf(`{"seq":%d,"type":"x","durable":false,"Durable":true}`, s) },
	} {
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
			st.rangeRows = func(a, b uint64, l int) []StoredEvent {
				return []StoredEvent{{Seq: a + 1, Payload: []byte(payload(a + 1))}}
			}
			st.mu.Unlock()
			s.Backfill.batch = 1
			if evs, err := s.Backfill.Next(); !errors.Is(err, ErrBadStoredEvent) || len(evs) != 0 {
				t.Fatalf("大文字小文字違いのキーを持つ行が配られた: evs=%d err=%v", len(evs), err)
			}
		})
	}
}

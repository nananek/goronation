package opencode

import (
	"encoding/json"
	"testing"
)

// golden に現れる全ての SSE の type が、eventKinds にある (扱いを決めていない type が増えたら赤)。逆に、eventKinds にあるのに
// golden に無い type も、赤 (採取の裏づけの無い規則を、残さない)。
func TestEveryGoldenEventTypeIsClassified(t *testing.T) {
	seen := map[string]bool{}
	for _, scene := range scenes(t) {
		for _, l := range readLines(t, scene, "events.ndjson") {
			var e struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatal(err)
			}
			seen[e.Type] = true
			if _, ok := eventKinds[e.Type]; !ok {
				t.Errorf("%s: SSE の type %q の扱いが、eventKinds に無い", scene, e.Type)
			}
		}
	}
	for typ := range eventKinds {
		if !seen[typ] {
			t.Errorf("eventKinds の %q が、golden に無い", typ)
		}
	}
	if len(seen) != 43 {
		t.Errorf("golden の type は %d 種 (PR④ の採取は 43 種)", len(seen))
	}
}

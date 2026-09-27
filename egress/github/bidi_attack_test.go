package github

import "testing"

// TestParsePullRejectsEveryBidiControl は、doc・PR 本文の「双方向の制御文字は断る」が、Unicode の Bidi_Control の全て
// (ALM・LRM・RLM・LRE・RLE・PDF・LRO・RLO・LRI・RLI・FSI・PDI) に当たることを確かめる。
// checkText は 202A-202E と 2066-2069 だけを見ており、061C・200E・200F が、title・body を通る。
func TestParsePullRejectsEveryBidiControl(t *testing.T) {
	pp := policyFor(t, repoOR)
	bidi := []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069}
	for _, r := range bidi {
		s := "a" + string(r) + "b"
		for _, field := range []string{"title", "body"} {
			var kv []any
			if field == "title" {
				kv = []any{"title", s, "head", headOf("x")}
			} else {
				kv = []any{"title", "t", "body", s, "head", headOf("x")}
			}
			_, err := pp.ParsePull(repoOR, []byte(pullBody(t, kv...)))
			if Reason(err) != CodeBadField {
				t.Errorf("U+%04X を %s に入れた要求: Reason = %q (err = %v), 期待 %q", r, field, Reason(err), err, CodeBadField)
			}
		}
	}
}

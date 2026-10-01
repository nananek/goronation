package chat

import (
	"errors"
	"io"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// 本番の既定 (SessionConfig のゼロ値) は、content_hash を必須にする (ADR 0042 決定 3)。OptionalContentHash を立てたときだけ、無くても通る。
func TestSessionContentHashRequiredByDefault(t *testing.T) {
	for _, tc := range []struct {
		name     string
		optional bool
	}{{"既定 (必須)", false}, {"OptionalContentHash", true}} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := Agent("claude")
			if err != nil {
				t.Fatal(err)
			}
			s := NewSession(SessionConfig{Launch: l, ID: "s", Input: io.Discard, OptionalContentHash: tc.optional})
			if err := s.Conv.Send("go"); err != nil {
				t.Fatal(err)
			}
			s.ReadOutput(strings.NewReader(string(reqFrame("p1")) + "\n"))
			if s.Conv.Pending() != 1 {
				t.Fatalf("pending = %d", s.Conv.Pending())
			}
			err = s.Conv.ResolveIn(s.Conv.Generation(), "p1", v0.AllowOnce)
			switch {
			case tc.optional && err != nil:
				t.Errorf("hash 無しが、任意の設定で通らない: %v", err)
			case !tc.optional && !errors.Is(err, ErrContentHashRequired):
				t.Errorf("hash 無しが、既定で通った・別の error: %v (ErrContentHashRequired のはず)", err)
			}
			if !tc.optional && s.Conv.Pending() != 1 {
				t.Errorf("拒否された応答で、未決が動いた: pending=%d", s.Conv.Pending())
			}
		})
	}
}

// hash を持たない保留の要求は、承認できない側に倒れる (fail-closed): 応答の hash が無くても (必須)・あっても (保持した値が無いので不一致) 通らず、未決のまま。
func TestHashlessPendingRequestCannotBeResolved(t *testing.T) {
	e := newConvWith(t, HubConfig{}, nil, true)
	e.c.mu.Lock()
	e.c.pending = map[string]pendingItem{"p1": {ev: Event{Type: v0.TypePermissionRequested, Seq: 1}}} // hash が空
	e.c.mu.Unlock()
	for _, got := range []string{"", "sha256:00", "sha256:" + strings.Repeat("0", 64)} {
		err := e.c.ResolveIn(e.c.Generation(), "p1", v0.AllowOnce)
		if got != "" {
			err = e.c.ResolvePermissionIn(e.c.Generation(), v0.PermissionResolve{RequestID: "p1", Outcome: v0.AllowOnce, ContentHash: got})
		}
		want := ErrContentHashRequired
		if got != "" {
			want = ErrContentChanged
		}
		if !errors.Is(err, want) {
			t.Errorf("got=%q: err = %v (%v のはず)", got, err, want)
		}
	}
	if e.c.Pending() != 1 || len(e.written()) != 0 {
		t.Errorf("hash の無い要求が、承認された: pending=%d writes=%q", e.c.Pending(), e.written())
	}
}

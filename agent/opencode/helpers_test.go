package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// sse は、SSE の 1 行 (type と data の JSON) を作る。data は、JSON の文字列。
func sse(typ, data string) []byte {
	return []byte(fmt.Sprintf(`{"id":"evt_1","type":%q,"data":%s}`, typ, data))
}

func feed(t testing.TB, s *Stream, typ, data string) []v0.Envelope {
	t.Helper()
	envs, err := s.DecodeFrame(sse(typ, data))
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	return envs
}

// started は、root session ses_root を作った Stream。
func started(t testing.TB) *Stream {
	t.Helper()
	s := &Stream{}
	envs := feed(t, s, "session.created", `{"sessionID":"ses_root","location":{"directory":"/work"},"version":"2.0.20"}`)
	if len(envs) != 1 || envs[0].Type != v0.TypeSessionStarted {
		t.Fatalf("session.created: %v", types(envs))
	}
	return s
}

// child は、子 session を足す。
func child_(t testing.TB, s *Stream, id, parent string) {
	t.Helper()
	if envs := feed(t, s, "session.created", fmt.Sprintf(`{"sessionID":%q,"parentID":%q,"agent":"general"}`, id, parent)); len(envs) != 0 {
		t.Fatalf("子 session: %v", types(envs))
	}
}

func asked(id, sid, action, resources, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"id":%q,"sessionID":%q,"action":%q,"resources":%s,"source":{"id":"call_1"}%s}`, id, sid, action, resources, extra)
}

func cmdPermission(t testing.TB, s *Stream, id, outcome string) ([]byte, []v0.Envelope, error) {
	t.Helper()
	d, _ := json.Marshal(map[string]string{"request_id": id, "outcome": outcome})
	return s.EncodeCommand(v0.Command{Type: v0.CommandPermissionResolve, Data: d})
}

func cmdForm(t testing.TB, s *Stream, r v0.FormResolve) ([]byte, []v0.Envelope, error) {
	t.Helper()
	d, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return s.EncodeCommand(v0.Command{Type: v0.CommandFormResolve, Data: d})
}

func dataOf(t testing.TB, e v0.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("%v: %s", err, e.Data)
	}
	return m
}

func mustTypes(t testing.TB, envs []v0.Envelope, want ...string) {
	t.Helper()
	if got := strings.Join(types(envs), ","); got != strings.Join(want, ",") {
		t.Fatalf("types = %s, want %s", got, strings.Join(want, ","))
	}
}

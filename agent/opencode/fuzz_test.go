package opencode

import (
	"encoding/json"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// FuzzDecodeFrame は、任意のバイト列 (root を作った後の SSE の 1 行) で、(1) panic せず、(2) 出す Envelope の data が JSON で、
// (3) permission.requested は Validate を通り、form.requested も通り、(4) 保持した未決の要求への応答が、panic せず、JSON の 1 行を返すことを確かめる。
func FuzzDecodeFrame(f *testing.F) {
	for _, scene := range []string{"simple-text", "subagent", "question-multiple", "error-provider-500", "tool-call-shell-allow", "interrupt-pending-permission"} {
		for _, l := range readLines(f, scene, "events.ndjson") {
			f.Add(l)
		}
	}
	f.Add(sse("permission.asked", asked("per_x", "ses_root", "shell", `["a","b"]`, `"metadata":{"files":[{"file":"f","patch":"p"}]}`)))
	f.Add(sse("form.created", formCreatedData("frm_x", "ses_root", colorField)))
	f.Fuzz(func(t *testing.T, line []byte) {
		s := &Stream{}
		_, _ = s.DecodeFrame(sse("session.created", `{"sessionID":"ses_root"}`))
		envs, err := s.DecodeFrame(line)
		if err != nil {
			return
		}
		for _, e := range envs {
			var v any
			if json.Unmarshal(e.Data, &v) != nil {
				t.Fatalf("%s の data が JSON でない: %s", e.Type, e.Data)
			}
			switch e.Type {
			case v0.TypePermissionRequested:
				var p v0.PermissionRequested
				if json.Unmarshal(e.Data, &p) != nil || p.Validate() != nil {
					t.Fatalf("Validate を通らない permission.requested: %s", e.Data)
				}
			case v0.TypeFormRequested:
				var fr v0.FormRequested
				if json.Unmarshal(e.Data, &fr) != nil || fr.Validate() != nil {
					t.Fatalf("Validate を通らない form.requested: %s", e.Data)
				}
			}
		}
		if len(s.pending)+len(s.forms) > maxPending {
			t.Fatal("未決が上限を超えた")
		}
		for id := range s.pending {
			d, _ := json.Marshal(map[string]string{"request_id": id, "outcome": v0.RejectOnce})
			out, _, err := s.EncodeCommand(v0.Command{Type: v0.CommandPermissionResolve, Data: d})
			var r httpRequest
			if err != nil || json.Unmarshal(out, &r) != nil || r.Method != "POST" {
				t.Fatalf("拒否が返せない: %v %s", err, out)
			}
		}
		for id := range s.forms {
			d, _ := json.Marshal(v0.FormResolve{RequestID: id, Outcome: v0.FormCancelled})
			out, _, err := s.EncodeCommand(v0.Command{Type: v0.CommandFormResolve, Data: d})
			var r httpRequest
			if err != nil || json.Unmarshal(out, &r) != nil || r.Method != "DELETE" {
				t.Fatalf("取り消しが返せない: %v %s", err, out)
			}
		}
	})
}

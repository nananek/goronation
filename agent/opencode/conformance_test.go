package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// TestGoldenSnapshots は、golden の全場面を再生した出力 (testdata/<場面>.v0.ndjson) を固定する。更新は go test -update (差分をレビューする)。
func TestGoldenSnapshots(t *testing.T) {
	for _, scene := range scenes(t) {
		t.Run(scene, func(t *testing.T) {
			var buf bytes.Buffer
			for _, e := range replay(t, scene).envs {
				buf.Write(snapshotLine(e))
				buf.WriteByte('\n')
			}
			path := filepath.Join("testdata", scene+".v0.ndjson")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (go test -update で作る)", err)
			}
			if !bytes.Equal(want, buf.Bytes()) {
				t.Errorf("%s が、現在の出力と違う (go test -update で更新し、差分をレビューする)", path)
			}
		})
	}
}

// 場面ごとに、期待する事象を、明示する (snapshot を、うのみにしない)。
func TestSceneExpectations(t *testing.T) {
	has := func(envs []v0.Envelope, typ string) int {
		n := 0
		for _, e := range envs {
			if e.Type == typ {
				n++
			}
		}
		return n
	}
	data := func(e v0.Envelope) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(e.Data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := func(envs []v0.Envelope, typ string) v0.Envelope {
		for _, e := range envs {
			if e.Type == typ {
				return e
			}
		}
		t.Fatalf("%s が無い: %v", typ, types(envs))
		return v0.Envelope{}
	}
	last := func(envs []v0.Envelope) v0.Envelope { return envs[len(envs)-1] }

	t.Run("simple-text", func(t *testing.T) {
		envs := decodeAll(t, "simple-text")
		if got := types(envs); got[0] != v0.TypeSessionStarted && got[0] != v0.TypeTurnStarted {
			t.Fatalf("%v", got)
		}
		if has(envs, v0.TypeSessionStarted) != 1 || has(envs, v0.TypeMessageText) != 1 || has(envs, v0.TypeUsage) < 1 ||
			last(envs).Type != v0.TypeTurnCompleted || data(last(envs))["stop_reason"] != v0.StopEndTurn {
			t.Fatalf("%v", types(envs))
		}
		if data(first(envs, v0.TypeSessionStarted))["agent"] != Name || data(first(envs, v0.TypeSessionStarted))["agent_session"] != "ses_1" {
			t.Fatalf("%s", first(envs, v0.TypeSessionStarted).Data)
		}
	})
	t.Run("tool-call-shell-allow", func(t *testing.T) {
		envs := decodeAll(t, "tool-call-shell-allow")
		ts := types(envs)
		iReq, iRes := slices.Index(ts, v0.TypePermissionRequested), slices.Index(ts, v0.TypePermissionResolved)
		if iReq < 0 || iRes < iReq || has(envs, v0.TypePermissionResolved) != 1 || data(envs[iRes])["by"] != "human" || data(envs[iRes])["outcome"] != v0.AllowOnce {
			t.Fatalf("%v", ts)
		}
		var done bool
		for _, e := range envs {
			if e.Type == v0.TypeToolUpdate && data(e)["status"] == v0.ToolCompleted {
				done = true
			}
		}
		if !done || has(envs, v0.TypeToolCall) != 1 {
			t.Fatalf("%v", ts)
		}
	})
	t.Run("tool-call-shell-reject", func(t *testing.T) {
		envs := decodeAll(t, "tool-call-shell-reject")
		var failed bool
		for _, e := range envs {
			if e.Type == v0.TypeToolUpdate && data(e)["status"] == v0.ToolFailed {
				failed = true
			}
		}
		if !failed || data(first(envs, v0.TypePermissionResolved))["outcome"] != v0.RejectOnce || data(last(envs))["stop_reason"] != v0.StopCancelled {
			t.Fatalf("%v", types(envs))
		}
	})
	t.Run("tool-call-shell-always", func(t *testing.T) {
		envs := decodeAll(t, "tool-call-shell-always")
		// 別のクライアントが always を返したものは、by=agent・allow_always (アダプタ自身は always を出さない)。
		var n int
		for _, e := range envs {
			if e.Type == v0.TypePermissionResolved && data(e)["by"] == "agent" && data(e)["outcome"] == v0.AllowAlways {
				n++
			}
		}
		if n == 0 {
			t.Fatalf("%v", types(envs))
		}
	})
	t.Run("question-single", func(t *testing.T) {
		envs := decodeAll(t, "question-single")
		f := first(envs, v0.TypeFormRequested)
		var req v0.FormRequested
		if err := json.Unmarshal(f.Data, &req); err != nil || req.Kind != v0.FormKindQuestion || req.Validate() != nil {
			t.Fatalf("%v %s", err, f.Data)
		}
		r := first(envs, v0.TypeFormResolved)
		if data(r)["by"] != "human" || data(r)["outcome"] != v0.FormAnswered || has(envs, v0.TypeFormResolved) != 1 {
			t.Fatalf("%s", r.Data)
		}
	})
	t.Run("question-rule-allow", func(t *testing.T) {
		envs := decodeAll(t, "question-rule-allow")
		if has(envs, v0.TypePermissionRequested) != 0 || has(envs, v0.TypeFormRequested) != 1 {
			t.Fatalf("%v", types(envs))
		}
	})
	t.Run("question-cancel", func(t *testing.T) {
		envs := decodeAll(t, "question-cancel")
		if data(first(envs, v0.TypeFormResolved))["outcome"] != v0.FormCancelled || has(envs, v0.TypeFormResolved) != 1 {
			t.Fatalf("%v", types(envs))
		}
	})
	t.Run("websearch-form", func(t *testing.T) {
		envs := decodeAll(t, "websearch-form")
		var req v0.FormRequested
		if err := json.Unmarshal(first(envs, v0.TypeFormRequested).Data, &req); err != nil || req.Kind != "websearch.provider" {
			t.Fatalf("%v %+v", err, req)
		}
	})
	t.Run("subagent", func(t *testing.T) {
		envs := decodeAll(t, "subagent")
		var child, root int
		for _, e := range envs {
			if e.Type != v0.TypePermissionRequested {
				continue
			}
			if e.Origin != nil {
				child++
				if e.Origin.ID != "ses_2" || e.Origin.Parent != "call_sub" {
					t.Fatalf("origin = %+v", e.Origin)
				}
			} else {
				root++
			}
		}
		if child != 1 || root != 1 || has(envs, v0.TypeTurnCompleted) != 1 {
			t.Fatalf("child=%d root=%d turn.completed=%d", child, root, has(envs, v0.TypeTurnCompleted))
		}
		for _, e := range envs { // 子の発言・tool も、帰属つき。メインの出力は、帰属なし
			if (e.Type == v0.TypeMessageText || e.Type == v0.TypeToolCall) && string(e.Data) != "" {
				var m map[string]any
				_ = json.Unmarshal(e.Data, &m)
				if (m["text"] == "The directory is empty." || m["call_id"] == "call_child_shell") != (e.Origin != nil) {
					t.Fatalf("%s: origin = %+v", e.Data, e.Origin)
				}
			}
		}
	})
	t.Run("interrupt-streaming", func(t *testing.T) {
		envs := decodeAll(t, "interrupt-streaming")
		if data(last(envs))["stop_reason"] != v0.StopCancelled {
			t.Fatalf("%v", types(envs))
		}
	})
	t.Run("interrupt-pending-permission", func(t *testing.T) {
		envs := decodeAll(t, "interrupt-pending-permission")
		ts := types(envs)
		iRes := slices.Index(ts, v0.TypePermissionResolved)
		if iRes < 0 || data(envs[iRes])["by"] != "agent" || data(envs[iRes])["outcome"] != "cancelled" || iRes > slices.Index(ts, v0.TypeTurnCompleted) {
			t.Fatalf("%v", ts)
		}
	})
	for scene, status := range map[string]float64{"error-provider-500": 500, "error-provider-400": 400} {
		t.Run(scene, func(t *testing.T) {
			envs := decodeAll(t, scene)
			e := first(envs, v0.TypeError)
			if data(e)["status"] != status || data(last(envs))["stop_reason"] != v0.StopError || data(last(envs))["is_error"] != true {
				t.Fatalf("%s %v", e.Data, types(envs))
			}
			if data(e)["retryable"] != (status >= 500) {
				t.Fatalf("retryable = %v", data(e)["retryable"])
			}
		})
	}
	t.Run("multi-turn", func(t *testing.T) {
		envs := decodeAll(t, "multi-turn")
		if has(envs, v0.TypeTurnStarted) < 2 || has(envs, v0.TypeTurnCompleted) != has(envs, v0.TypeTurnStarted) {
			t.Fatalf("%v", types(envs))
		}
	})
}

// 全場面で、要求 (permission.requested・form.requested) ごとに、決着 (*.resolved) が、ちょうど 1 つ付く (spec/v0 の約束)。
func TestEveryRequestIsSettledExactlyOnce(t *testing.T) {
	for _, scene := range scenes(t) {
		envs := decodeAll(t, scene)
		req, res := map[string]int{}, map[string]int{}
		for _, e := range envs {
			var d struct {
				RequestID string `json:"request_id"`
			}
			_ = json.Unmarshal(e.Data, &d)
			switch e.Type {
			case v0.TypePermissionRequested, v0.TypeFormRequested:
				req[e.Type+d.RequestID]++
			case v0.TypePermissionResolved:
				res[v0.TypePermissionRequested+d.RequestID]++
			case v0.TypeFormResolved:
				res[v0.TypeFormRequested+d.RequestID]++
			}
		}
		for k, n := range req {
			if n != 1 || res[k] != 1 {
				t.Errorf("%s: %s の要求 %d 回・決着 %d 回", scene, k, n, res[k])
			}
		}
		for k, n := range res {
			if req[k] != 1 {
				t.Errorf("%s: %s の決着 %d 回が、要求に対応しない", scene, k, n)
			}
		}
	}
}

// 場面の再生で EncodeCommand が返した HTTP 要求は、実際に opencode へ送った要求 (requests.ndjson) の 1 つと、method・path・body が一致する。
func TestEncodedRequestsMatchTheCapturedOnes(t *testing.T) {
	for _, scene := range scenes(t) {
		type captured struct {
			Method string          `json:"method"`
			Path   string          `json:"path"`
			Body   json.RawMessage `json:"body"`
		}
		var golden []captured
		for _, l := range readLines(t, scene, "requests.ndjson") {
			var c captured
			if err := json.Unmarshal(l, &c); err != nil {
				t.Fatal(err)
			}
			if len(c.Body) == 0 {
				c.Body = json.RawMessage("null")
			}
			golden = append(golden, c)
		}
		for _, r := range replay(t, scene).reqs {
			found := false
			for _, g := range golden {
				if g.Method == r.Method && g.Path == r.Path && jsonEqual(g.Body, r.Body) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: 出した要求 %s %s %s が、採取した要求に無い", scene, r.Method, r.Path, r.Body)
			}
		}
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ab, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(ab, bb)
}

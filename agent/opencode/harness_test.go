package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

var update = flag.Bool("update", false, "testdata/<場面>.v0.ndjson を、現在の出力で書き直す")

const goldenDir = "../../spec/testdata/golden/opencode-serve"

// golden の ID は、正規化の記号 (<ses:1>・<per:1>…)。アダプタの validID は、本物の形 (ses_…) だけを通すので、読み込みのときに
// <名前:番号> を 名前_番号 に戻す。
var tokenRe = regexp.MustCompile(`<([a-z]+):(\d+)>`)

func unnormalize(b []byte) []byte { return tokenRe.ReplaceAll(b, []byte("${1}_${2}")) }

func scenes(t testing.TB) []string {
	t.Helper()
	es, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func readLines(t testing.TB, scene, file string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, scene, file))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			out = append(out, unnormalize(bytes.Clone(sc.Bytes())))
		}
	}
	return out
}

// replayResult は、場面の再生の結果: 出た Envelope (合成を含む。到着順)・EncodeCommand が返した HTTP 要求。
type replayResult struct {
	envs []v0.Envelope
	reqs []httpRequest
}

// replay は、場面の SSE を 1 行ずつ DecodeFrame に通す。人間の操作が起こしたイベント (prompt の inbox・permission.replied・form.replied・
// form.cancelled) の直前に、対応する Command を EncodeCommand に渡す (本番で、HTTP 要求が SSE の事象より先に出るのと同じ順)。
func replay(t testing.TB, scene string) replayResult { return replayWith(t, scene, nil) }

func replayWith(t testing.TB, scene string, perFrame func([]v0.Envelope)) replayResult {
	t.Helper()
	s := &Stream{}
	var res replayResult
	cmd := func(typ string, data any) {
		b, _ := json.Marshal(data)
		line, syn, err := s.EncodeCommand(v0.Command{V: v0.Version, Type: typ, Data: b})
		if err != nil {
			t.Fatalf("%s: %s: %v", scene, typ, err)
		}
		var r httpRequest
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		res.reqs = append(res.reqs, r)
		res.envs = append(res.envs, syn...)
	}
	for _, line := range readLines(t, scene, "events.ndjson") {
		var e struct {
			Type string `json:"type"`
			Data struct {
				SessionID string `json:"sessionID"`
				RequestID string `json:"requestID"`
				ID        string `json:"id"`
				Reply     string `json:"reply"`
				Item      struct {
					Type    string `json:"type"`
					Payload struct {
						Text string `json:"text"`
					} `json:"payload"`
				} `json:"item"`
				Answer json.RawMessage `json:"answer"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case "session.inbox.enqueued":
			if e.Data.SessionID == s.root && e.Data.Item.Type == "user" {
				cmd(v0.CommandPrompt, map[string]string{"text": e.Data.Item.Payload.Text})
			}
		case "permission.replied":
			switch e.Data.Reply {
			case "once":
				cmd(v0.CommandPermissionResolve, map[string]string{"request_id": e.Data.RequestID, "outcome": v0.AllowOnce})
			case "reject":
				cmd(v0.CommandPermissionResolve, map[string]string{"request_id": e.Data.RequestID, "outcome": v0.RejectOnce})
			} // always は、アダプタが出さない (別のクライアントの返答として、by=agent になる)
		case "form.replied":
			var ans map[string]v0.FormValue
			if err := json.Unmarshal(e.Data.Answer, &ans); err != nil {
				t.Fatal(err)
			}
			cmd(v0.CommandFormResolve, v0.FormResolve{RequestID: e.Data.ID, Outcome: v0.FormAnswered, Answer: ans})
		case "form.cancelled":
			cmd(v0.CommandFormResolve, v0.FormResolve{RequestID: e.Data.ID, Outcome: v0.FormCancelled})
		}
		envs, err := s.DecodeFrame(line)
		if err != nil {
			t.Fatalf("%s: DecodeFrame: %v\n%s", scene, err, line)
		}
		res.envs = append(res.envs, envs...)
		if perFrame != nil {
			perFrame(envs)
		}
	}
	return res
}

// snapshotLine は、Envelope の snapshot の 1 行 (Raw は除く)。
func snapshotLine(e v0.Envelope) []byte {
	e.Raw = nil
	b, err := marshal(struct {
		Type    string          `json:"type"`
		Durable bool            `json:"durable"`
		Origin  *v0.Origin      `json:"origin,omitempty"`
		Data    json.RawMessage `json:"data"`
	}{e.Type, e.Durable, e.Origin, e.Data})
	if err != nil {
		panic(err)
	}
	return b
}

// replayPerFrame は、replay と同じ再生で、frame (events.ndjson の行) ごとに、その frame を DecodeFrame した Envelope を返す (Command の合成は含めない)。
func replayPerFrame(t testing.TB, scene string) [][]v0.Envelope {
	t.Helper()
	var per [][]v0.Envelope
	replayWith(t, scene, func(envs []v0.Envelope) { per = append(per, envs) })
	return per
}

func decodeAll(t testing.TB, scene string) []v0.Envelope { return replay(t, scene).envs }

func types(envs []v0.Envelope) []string {
	var out []string
	for _, e := range envs {
		out = append(out, e.Type)
	}
	return out
}

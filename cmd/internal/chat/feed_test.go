package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/agent/claude"
	"github.com/nananek/goronation/core/agent"
	v0 "github.com/nananek/goronation/spec/v0"
)

var fixedNow = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 123456789, time.FixedZone("JST", 9*3600)) }

// leakStream は、Raw と、Feed が決めるべき値 (ID・TS・Session・Seq) を、わざと埋めて返す Stream。
type leakStream struct{ decodeErr error }

func (s leakStream) DecodeFrame(raw []byte) ([]v0.Envelope, error) {
	if s.decodeErr != nil {
		return nil, s.decodeErr
	}
	return []v0.Envelope{{V: 99, ID: "evil", TS: "evil", Session: "evil", Seq: 999, Type: v0.TypeAgentFrame, Data: json.RawMessage(`{}`),
		Raw: json.RawMessage(`{"secret":"CANARY-RAW"}`)}}, nil
}

func (leakStream) EncodeCommand(v0.Command) ([]byte, []v0.Envelope, error) {
	return []byte("x\n"), []v0.Envelope{{Type: v0.TypeTurnStarted, Durable: true, Raw: json.RawMessage(`"CANARY-RAW"`)}}, nil
}

func TestFeedStampsAndDropsRaw(t *testing.T) {
	f := NewFeed(leakStream{}, "sess-1", fixedNow)
	for i := 0; i < 3; i++ {
		evs, err := f.Decode([]byte(`{}`))
		if err != nil || len(evs) != 1 {
			t.Fatalf("evs=%v err=%v", evs, err)
		}
		if bytes.Contains(evs[0].JSON, []byte("CANARY-RAW")) || bytes.Contains(evs[0].JSON, []byte(`"raw"`)) {
			t.Fatalf("raw が出た: %s", evs[0].JSON)
		}
		var m map[string]any
		if err := json.Unmarshal(evs[0].JSON, &m); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"v": 0.0, "id": fmt.Sprintf("e%d", i), "ts": "2026-09-29T03:00:00.123Z", "session": "sess-1", "seq": float64(i),
			"type": v0.TypeAgentFrame, "durable": false, "data": map[string]any{}}
		if fmt.Sprint(m) != fmt.Sprint(want) || evs[0].Seq != uint64(i) {
			t.Fatalf("got %v\nwant %v", m, want)
		}
	}
	raw, evs, err := f.Encode(v0.Command{Type: v0.CommandPrompt})
	if err != nil || string(raw) != "x\n" || len(evs) != 1 || evs[0].Seq != 3 || !evs[0].Durable ||
		bytes.Contains(evs[0].JSON, []byte("CANARY-RAW")) || !strings.Contains(string(evs[0].JSON), `"data":{}`) {
		t.Fatalf("raw=%q evs=%+v err=%v", raw, evs, err)
	}
}

func TestFeedErrorDoesNotConsumeSeq(t *testing.T) {
	f := NewFeed(leakStream{decodeErr: fmt.Errorf("bad")}, "s", fixedNow)
	if _, err := f.Decode([]byte(`x`)); err == nil {
		t.Fatal("error が返らない")
	}
	f.stream = leakStream{}
	evs, _ := f.Decode([]byte(`{}`))
	if evs[0].Seq != 0 {
		t.Fatalf("seq=%d", evs[0].Seq)
	}
}

// 敵対的な text (改行・CR・U+2028・U+2029・</script>) が、JSON の 1 行に収まり、別のイベントを偽造できない。
func TestFeedJSONIsSingleLine(t *testing.T) {
	f := NewFeed(claude.Adapter{}.NewStream(), "s", fixedNow)
	text := "a\nb\rc\u2028d\u2029e</script>\ndata: {\"seq\":1}\n\nevent: end"
	frame, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "text", "text": text}}}})
	evs, err := f.Decode(frame)
	if err != nil || len(evs) == 0 {
		t.Fatalf("evs=%v err=%v", evs, err)
	}
	for _, e := range evs {
		if bytes.ContainsAny(e.JSON, "\n\r") || strings.ContainsAny(string(e.JSON), "\u2028\u2029") || bytes.Contains(e.JSON, []byte("</script>")) {
			t.Fatalf("1 行でない・エスケープされていない: %q", e.JSON)
		}
	}
	var got struct{ Data struct{ Text string } }
	if err := json.Unmarshal(evs[0].JSON, &got); err != nil || got.Data.Text != text {
		t.Fatalf("text が元に戻らない: %q err=%v", got.Data.Text, err)
	}
}

var publicKeys = []string{"data", "durable", "id", "seq", "session", "ts", "type", "v"}

func goldenClaudeFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "spec", "testdata", "golden", "claude", "*.jsonl"))
	if err != nil || len(files) < 8 {
		t.Fatalf("golden が見つからない: %v %v", files, err)
	}
	return files
}

// canary: golden の全フレームを、LineReader → Feed (claude のアダプタ) に通して、出力に raw が出ない。
// 出力のキーは封筒の語彙だけ。元のフレームの全文 (raw の中身) も、どこにも現れない。
func TestGoldenNeverExposesRaw(t *testing.T) {
	total := 0
	for _, file := range goldenClaudeFiles(t) {
		fh, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		lr := NewLineReader(fh, 0)
		f := NewFeed(claude.Adapter{}.NewStream(), "s", fixedNow)
		var next uint64
		for {
			line, err := lr.Next()
			if err != nil {
				break
			}
			evs, err := f.Decode(line)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			for _, e := range evs {
				total++
				if e.Seq != next {
					t.Fatalf("%s: seq が連続でない: %d want %d", file, e.Seq, next)
				}
				next++
				var m map[string]json.RawMessage
				if err := json.Unmarshal(e.JSON, &m); err != nil {
					t.Fatal(err)
				}
				var keys []string
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if strings.Join(keys, ",") != strings.Join(publicKeys, ",") {
					t.Fatalf("%s: 出力のキー = %v", file, keys)
				}
				if bytes.Contains(e.JSON, line) {
					t.Fatalf("%s: 元のフレームの全文が出た: %s", file, e.JSON)
				}
			}
		}
		fh.Close()
	}
	if total < 50 {
		t.Fatalf("イベントが少なすぎる (golden を読めていない): %d", total)
	}
}

// 標準形式の型が変わっても、Public の出力のキーが増えたら気づく: UIEnvelope のキーを、コードから数える。
func TestPublicKeysAreThePinnedSet(t *testing.T) {
	b, _ := json.Marshal(v0.UIEnvelope{})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(publicKeys, ",") {
		t.Fatalf("UIEnvelope のキーが変わった: %v (raw が増えていないか確かめて、publicKeys を更新する)", keys)
	}
}

func FuzzFeedClaude(f *testing.F) {
	for _, file := range []string{"simple-text", "tool-call", "permission-interactive-allow"} {
		b, _ := os.ReadFile(filepath.Join("..", "..", "..", "spec", "testdata", "golden", "claude", file+".jsonl"))
		for _, l := range bytes.Split(b, []byte("\n")) {
			f.Add(l)
		}
	}
	f.Add([]byte(`{"type":"control_request","request_id":"r","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		var s agent.Stream = claude.Adapter{}.NewStream()
		feed := NewFeed(s, "s", fixedNow)
		for i := 0; i < 2; i++ {
			evs, err := feed.Decode(line)
			if err != nil {
				return
			}
			for _, e := range evs {
				if bytes.ContainsAny(e.JSON, "\n\r") {
					t.Fatalf("不正な出力: %q (入力 %q)", e.JSON, line)
				}
				var m map[string]json.RawMessage
				if json.Unmarshal(e.JSON, &m) != nil || len(m) != len(publicKeys) {
					t.Fatalf("キーが封筒の語彙でない: %q", e.JSON)
				}
			}
		}
	})
}

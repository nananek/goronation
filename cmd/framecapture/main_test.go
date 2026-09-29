//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestParseCaptureArgsBasic(t *testing.T) {
	var stderr bytes.Buffer
	o, err := parseCaptureArgs("opencode", []string{
		"--goronation-bin", "/x/goronation", "--agent-bin", "/x/opencode", "--out", "/tmp/out.ndjson",
		"--", "hello", "world",
	}, &stderr)
	if err != nil {
		t.Fatalf("parseCaptureArgs: %v (%s)", err, stderr.String())
	}
	if o.goronationBin != "/x/goronation" || o.agentBin != "/x/opencode" || o.out != "/tmp/out.ndjson" {
		t.Fatalf("o = %+v", o)
	}
	if !slices.Equal(o.message, []string{"hello", "world"}) {
		t.Fatalf("message = %+v", o.message)
	}
}

func TestParseCaptureArgsDefaultResponse(t *testing.T) {
	var stderr bytes.Buffer
	o, err := parseCaptureArgs("opencode", []string{"--", "hi"}, &stderr)
	if err != nil {
		t.Fatalf("parseCaptureArgs: %v", err)
	}
	if o.response == "" {
		t.Fatal("--response を省略したのに、既定値が空")
	}
}

func TestParseCaptureArgsRequiresMessage(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseCaptureArgs("opencode", []string{"--out", "/tmp/x"}, &stderr)
	if err == nil {
		t.Fatal("-- の後ろにメッセージが無いのに、成功してしまった")
	}
}

func TestParseCaptureArgsNoDoubleDashMeansNoMessage(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseCaptureArgs("opencode", []string{"hello"}, &stderr)
	if err == nil {
		t.Fatal("-- が無い (フラグとして解釈できない語) のに、成功してしまった")
	}
}

func TestRunDispatchesToUnknownAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"unknown-agent", "--", "hi"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("未知のエージェントなのに、エラーを何も出していない")
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("-h なのに、使い方を出していない")
	}
}

func TestRunNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestParseCaptureArgsScenarioNeedsNoMessage(t *testing.T) {
	var stderr bytes.Buffer
	o, err := parseCaptureArgs("claude", []string{"--scenario", "s.json", "--normalize", "--timeout", "3s"}, &stderr)
	if err != nil {
		t.Fatalf("parseCaptureArgs: %v (%s)", err, stderr.String())
	}
	if o.scenario != "s.json" || !o.normalize || o.timeout != 3*time.Second {
		t.Fatalf("o = %+v", o)
	}
}

func TestParseCaptureArgsDefaultTimeout(t *testing.T) {
	o, err := parseCaptureArgs("claude", []string{"--", "hi"}, io.Discard)
	if err != nil || o.timeout <= 0 {
		t.Fatalf("timeout = %v, err = %v", o.timeout, err)
	}
}

func TestNewPlanWithoutScenarioJoinsMessage(t *testing.T) {
	p, err := newPlan(captureOptions{message: []string{"a", "b"}}, "claude")
	if err != nil || !slices.Equal(p.turns, []string{"a b"}) || p.sc != nil || p.expectExit != 0 {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

func TestNewPlanPicksExpectedExitPerAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	body := `{"turns":["t1","t2"],"expect_exit":{"claude":1,"opencode":2},"claude":[{"text":"x"}],"opencode":[{"content":"y"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pc, err := newPlan(captureOptions{scenario: path}, "claude")
	if err != nil || pc.expectExit != 1 || !slices.Equal(pc.turns, []string{"t1", "t2"}) {
		t.Fatalf("claude: %+v, %v", pc, err)
	}
	po, err := newPlan(captureOptions{scenario: path}, "opencode")
	if err != nil || po.expectExit != 2 {
		t.Fatalf("opencode: %+v, %v", po, err)
	}
}

func TestFinishTreatsExpectedExitAsSuccess(t *testing.T) {
	var out, errb bytes.Buffer
	o := captureOptions{}
	if c := finish(o, "claude", plan{expectExit: 1}, 1, []byte("{}\n"), &out, &errb); c != 0 {
		t.Errorf("期待した終了コード 1 が、%d になった", c)
	}
	if c := finish(o, "claude", plan{expectExit: 0}, 1, []byte("{}\n"), &out, &errb); c != 1 {
		t.Errorf("期待外の終了コード 1 が、%d になった", c)
	}
	if c := finish(o, "claude", plan{expectExit: 1}, 0, []byte("{}\n"), &out, &errb); c == 0 {
		t.Errorf("1 を期待したのに 0 で終わったのが、成功 (0) になった")
	}
}

func TestEmitNormalizesToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.ndjson")
	raw := []byte(`{"timestamp":1790644836079,"sessionID":"ses_abc"}` + "\n")
	if err := emit(captureOptions{out: path, normalize: true}, raw, io.Discard); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != `{"sessionID":"<ses:1>","timestamp":0}`+"\n" {
		t.Fatalf("got %q", got)
	}
	// --normalize が無ければ、そのまま書く。
	var stdout bytes.Buffer
	if err := emit(captureOptions{}, raw, &stdout); err != nil || stdout.String() != string(raw) {
		t.Fatalf("stdout = %q, %v", stdout.String(), err)
	}
}

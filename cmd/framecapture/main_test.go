//go:build linux

package main

import (
	"bytes"
	"slices"
	"testing"
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

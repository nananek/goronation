package egress

import (
	"io"
	"slices"
	"testing"
)

// TestClaudeHosts は、claude が要る宛先の一覧 (spike の実測で確定したもの) を固定する。
// 足すときは、必要と分かった根拠を添えて、この表も変える。
func TestClaudeHosts(t *testing.T) {
	want := []string{"api.anthropic.com:443", "platform.claude.com:443"}
	got := ClaudeHosts()
	if !slices.Equal(got, want) {
		t.Fatalf("ClaudeHosts() = %v, want %v", got, want)
	}
	// 呼び出し側が書き換えても、次の呼び出しには影響しない。
	got[0] = "evil.example:443"
	if again := ClaudeHosts(); !slices.Equal(again, want) {
		t.Errorf("書き換えが伝わった: %v", again)
	}
	// そのまま Allow に渡せる。
	s := New(Config{Allow: ClaudeHosts(), Audit: io.Discard})
	if s.err != nil {
		t.Fatalf("Allow に渡せない: %v", s.err)
	}
	for _, h := range want {
		if _, ok := s.allow[h]; !ok {
			t.Errorf("許可の表に %s が無い", h)
		}
	}
	if len(s.allow) != len(want) {
		t.Errorf("許可の表 = %v", s.allow)
	}
}

// TestClaudeHostsRejectOthers は、ClaudeHosts の許可で、claude が要らない宛先 (raw.githubusercontent.com など) が断られることを固定する。
func TestClaudeHostsRejectOthers(t *testing.T) {
	p := startProxy(t, Config{Allow: ClaudeHosts()}, nil)
	for _, target := range []string{
		"raw.githubusercontent.com:443", "http-intake.logs.datadoghq.com:443", "api.anthropic.com:80", "api.anthropic.com:8443",
		"platform.claude.com:80", "claude.ai:443", "anthropic.com:443", "evil.api.anthropic.com:443",
	} {
		if r, _, _ := p.request(t, connectRequest(target)); r.code != 403 {
			t.Errorf("CONNECT %s = %d, want 403", target, r.code)
		}
	}
	for _, r := range p.audit.records(t) {
		if r.Reason != reasonNotAllowed || r.Target == "" {
			t.Errorf("監査 = %+v (拒否した宛先が、ログに出るはず)", r)
		}
	}
}

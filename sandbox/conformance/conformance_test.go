package conformance

import (
	"slices"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
)

// TestMinimalSpecLayout は、Spec の配置を確認する: PathRemap のバックエンドは論理的な配置 (GuestPath あり)、それ以外はホストと同じ path。
func TestMinimalSpecLayout(t *testing.T) {
	remap := minimalSpec(sandbox.Capabilities{PathRemap: true}, "/host/probe", "-x")
	if remap.Exec != "/opt/probe/probe" || remap.Read[0].HostPath != "/host/probe" || remap.Read[0].GuestPath != "/opt/probe/probe" || !remap.System {
		t.Errorf("PathRemap: %+v", remap)
	}
	same := minimalSpec(sandbox.Capabilities{}, "/host/probe", "-x")
	if same.Exec != "/host/probe" || same.Read[0].GuestPath != "" || same.Read[0].Guest() != "/host/probe" {
		t.Errorf("PathRemap でない: %+v", same)
	}
	for _, s := range []sandbox.Spec{remap, same} {
		if !slices.Equal(s.Args, []string{probeArg, "-x"}) {
			t.Errorf("Args = %q, want [%s -x]", s.Args, probeArg)
		}
	}
	c := &C{caps: sandbox.Capabilities{PathRemap: true}}
	if c.guest("/h", "/l") != "/l" || c.mount("/h", "/l").GuestPath != "/l" {
		t.Error("PathRemap の guest・mount")
	}
	c.caps.PathRemap = false
	if c.guest("/h", "/l") != "/h" || c.mount("/h", "/l").GuestPath != "" {
		t.Error("PathRemap でない guest・mount")
	}
}

// TestListNames と TestPortOf は、probe の出力の読み取りを確認する。
func TestListNames(t *testing.T) {
	if n, ok := listNames("ok:a,b"); !ok || !slices.Equal(n, []string{"a", "b"}) {
		t.Errorf("ok:a,b = %q, %v", n, ok)
	}
	if n, ok := listNames("ok:"); !ok || len(n) != 0 {
		t.Errorf("ok: = %q, %v", n, ok)
	}
	if _, ok := listNames("err: no such file"); ok {
		t.Error("err: は、一覧できなかったこと")
	}
}

func TestPortOf(t *testing.T) {
	if n, ok := portOf("port 12345"); !ok || n != 12345 {
		t.Errorf("port 12345 = %d, %v", n, ok)
	}
	for _, s := range []string{"port x", "ready", "", "port"} {
		if _, ok := portOf(s); ok {
			t.Errorf("portOf(%q) が成功した", s)
		}
	}
}

// TestChecksTable は、項目の表の妥当性を確認する: id が重ならず、Cap つきの項目の Cap が、Capabilities の項目 (Has) を持つ。
func TestChecksTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range p0Checks {
		if c.id == "" || c.run == nil || seen[c.id] {
			t.Errorf("P0 の項目 %q が、空・run が無い・重複", c.id)
		}
		seen[c.id] = true
	}
	for _, c := range capChecks {
		if c.id == "" || c.run == nil || seen["cap-"+c.id] {
			t.Errorf("Cap つきの項目 %q が、空・run が無い・重複", c.id)
		}
		seen["cap-"+c.id] = true
		if !(sandbox.Capabilities{PathRemap: true, PrivateLoopback: true, KillsDescendants: true, PrivatePIDs: true}).Has(c.cap) {
			t.Errorf("Cap %q が、Capabilities.Has に無い", c.cap)
		}
	}
	// 契約の文言ごとの P0 が、揃っている (項目を消すと、ここが赤くなる)。
	for _, id := range []string{"hidden-host", "env-clean", "network-none", "egress-only", "write-scope", "ro-in-rw", "parent-death", "exit-status", "signal", "inherited-fd", "terminal", "rejects"} {
		if !seen[id] {
			t.Errorf("P0 の項目 %q が無い", id)
		}
	}
}

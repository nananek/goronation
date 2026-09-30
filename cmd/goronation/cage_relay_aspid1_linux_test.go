//go:build linux

package main

import "testing"

// RelayPort の檻は、init を PID 1 にして (--as-pid-1)、control (init の標準入力) を bwrap に持たせない。
// 欠くと、bwrap が PID 1 のまま control を dumpable で持ち、同じ uid の子が pidfd_getfd で奪える
// (実測: 攻撃者視点レビュー a3c2a8f9。孫が fd を奪い、ホストの要求を横取り・応答を偽造できた)。
func TestCageSpecRelayPortForcesAsPID1(t *testing.T) {
	for _, tc := range []struct {
		name string
		nd   bool
	}{
		{"RelayPort だけ", false},
		{"NonDumpable つき", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCage()
			c.RelayPort = 4321
			c.NonDumpable = tc.nd
			if spec := cageSpec(c); !spec.AsPID1 {
				t.Errorf("RelayPort があるのに AsPID1 が立っていない: PID 1 の bwrap が control を dumpable のまま持ち、pidfd_getfd で奪われる")
			}
		})
	}
	if spec := cageSpec(testCage()); spec.AsPID1 {
		t.Error("RelayPort も NonDumpable も無い既定の檻で AsPID1 が立っている")
	}
}

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// teardown は、檻の取り消しを先にし (標準入出力・egress はまだ生きている)、そのあと標準入出力と egress を閉じる。
func TestChatCageTeardownOrder(t *testing.T) {
	runDir, err := os.MkdirTemp("", "td")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })
	proxy, err := startProxy(runDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := newStdioPair()
	if err != nil {
		proxy.Close()
		t.Fatal(err)
	}
	cancelled := 0
	c := &chatCage{proxy: proxy, pair: pair, cancel: func() {
		cancelled++
		if _, err := pair.In.Write([]byte("x")); err != nil {
			t.Errorf("cancel の時点で、標準入出力が閉じている: %v", err)
		}
		if _, err := os.Stat(filepath.Join(runDir, proxySockName)); err != nil {
			t.Errorf("cancel の時点で、egress が止まっている: %v", err)
		}
	}}
	c.teardown()
	if cancelled != 1 {
		t.Fatalf("cancel = %d 回", cancelled)
	}
	if _, err := pair.In.Write([]byte("x")); err == nil {
		t.Error("teardown の後も、標準入出力が開いている")
	}
	if _, err := os.Stat(filepath.Join(runDir, proxySockName)); err == nil {
		t.Error("teardown の後も、egress の socket が残っている")
	}
}

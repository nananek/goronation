package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 読めない複製の作成が、途中で kill された (SIGKILL・電源断) 残りの一時ファイル (.tmp-*。複製と同じ大きさ) は、次の作成の掃除で消える。
// sweepExeCopies は、. で始まる名前 (lock・一時ファイル) を、すべて飛ばすので、クラッシュのたびに、複製 1 つ分 (約 240 MB) が、ずっと残る。
func TestUnreadableExeCopySweepsStaleTmp(t *testing.T) {
	src := t.TempDir()
	exe := filepath.Join(src, "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/true\n"+string(make([]byte, 4096))), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(src, "exe")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".tmp-crashed")
	if err := os.WriteFile(stale, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := unreadableExeCopy(dir, exe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("3 時間前の一時ファイル (%s) が、掃除されずに残った", filepath.Base(stale))
	}
	// 使っている最中の一時ファイル (新しい) は、消さない
	fresh := filepath.Join(dir, ".tmp-inuse")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := unreadableExeCopy(dir, exe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("使っている最中 (新しい) の一時ファイルを消した: %v", err)
	}
}

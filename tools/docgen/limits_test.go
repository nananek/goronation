package main

import (
	"testing"
	"time"
)

// TestDefaultLimits は、実行時の予算の値を固定する。値を緩める変更は、この値と、doc.go の記述を一緒に直す
// (緩めたことが、レビューで見える)。
func TestDefaultLimits(t *testing.T) {
	want := limits{
		Timeout:       60 * time.Second,
		MaxEntries:    100_000,
		MaxFiles:      5_000,
		MaxFileBytes:  1 << 20,
		MaxTotalBytes: 64 << 20,
		MaxDepth:      64,
	}
	if defaultLimits != want {
		t.Errorf("defaultLimits = %+v, want %+v", defaultLimits, want)
	}
}

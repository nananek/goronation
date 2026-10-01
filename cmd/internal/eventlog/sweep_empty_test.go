package eventlog

import "testing"

func TestSweepEmptySessionsDir(t *testing.T) {
	r, n, err := SweepStale(t.TempDir())
	if r != 0 || n != 0 || err != nil {
		t.Fatal(r, n, err)
	}
}

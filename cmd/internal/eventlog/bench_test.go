package eventlog

import (
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestBenchADR0027 は、ADR 0027 の完了条件 1・3 の測定 (1 セッション 1,000 件/秒・payload 2 KB・上限による GC の発火を含む)。
// 時間がかかるので、EVENTLOG_BENCH=<秒> を付けたときだけ回す (例: EVENTLOG_BENCH=70 go test -run BenchADR0027 -v)。
// 書き手 (chat 側) と同じ形で使う: 10 ms ごとに 10 件を Apply し、payload の合計が上限 (64 MiB) を超えたら、Trim で 60 MiB に収める。
func TestBenchADR0027(t *testing.T) {
	v := os.Getenv("EVENTLOG_BENCH")
	if v == "" {
		t.Skip("EVENTLOG_BENCH が無い")
	}
	secs, _ := strconv.Atoi(v)
	if secs <= 0 {
		secs = 20
	}
	const (
		budget  = 64 << 20
		lowMark = 60 << 20
		batch   = 10
		size    = 2048
	)
	s := open(t, session(t))
	var lat, trimLat []time.Duration
	var total int64
	var maxDisk int64
	var seq uint64
	pin := uint64(0)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for n := 0; time.Now().Before(deadline); n++ {
		<-tick.C
		op := Op{Append: rows(seq, seq+batch, size)}
		if n%500 == 0 { // 時々、固定する・解除する (未決の権限要求の形)
			op.Pin = []uint64{seq}
			if pin != 0 {
				op.Unpin = []uint64{pin}
			}
			pin = seq
		}
		seq += batch
		t0 := time.Now()
		if err := s.Apply(op); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(t0))
		total += batch * size
		if total > budget {
			t0 := time.Now()
			if _, err := s.Trim(lowMark); err != nil {
				t.Fatal(err)
			}
			d := time.Since(t0)
			trimLat = append(trimLat, d)
			total = lowMark
		}
		if n%100 == 0 {
			if sz, _ := s.Size(); sz > maxDisk {
				maxDisk = sz
			}
		}
	}
	if sz, _ := s.Size(); sz > maxDisk {
		maxDisk = sz
	}
	report := func(name string, d []time.Duration) {
		if len(d) == 0 {
			t.Logf("%s: 0 件", name)
			return
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		q := func(p float64) time.Duration { return d[int(float64(len(d)-1)*p)] }
		over := 0
		for _, x := range d {
			if x > time.Second {
				over++
			}
		}
		t.Logf("%s: n=%d p50=%v p99=%v p99.9=%v max=%v 50ms超=%d 1秒超=%d", name, len(d), q(.5), q(.99), q(.999), d[len(d)-1], countOver(d, 50*time.Millisecond), over)
	}
	report("Apply (10 件/回)", lat)
	report("Trim", trimLat)
	for _, n := range dbFiles {
		if fi, err := s.root.Lstat(n); err == nil {
			t.Logf("  %s = %.1f MiB", n, float64(fi.Size())/(1<<20))
		}
	}
	var ps int
	s.r.QueryRow(`PRAGMA page_size`).Scan(&ps)
	t.Logf("page_size=%d", ps)
	t.Logf("書いた件数=%d payload=%d MiB  ディスクの最大 (DB+WAL+shm)=%.1f MiB  (上限 64 MiB)", seq, int64(seq)*size>>20, float64(maxDisk)/(1<<20))
}

func countOver(d []time.Duration, x time.Duration) int {
	n := 0
	for _, v := range d {
		if v > x {
			n++
		}
	}
	return n
}

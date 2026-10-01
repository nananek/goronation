package eventlog

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const gen = "0123456789abcdef0123456789abcdef"

func session(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "s")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, gen, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(true) })
	return s
}

func rows(from, to uint64, size int) []Row {
	var out []Row
	for i := from; i < to; i++ {
		out = append(out, Row{Seq: i, Payload: bytes.Repeat([]byte{'a' + byte(i%26)}, size)})
	}
	return out
}

func seqs(rs []Row) []uint64 {
	var out []uint64
	for _, r := range rs {
		out = append(out, r.Seq)
	}
	return out
}

func eq(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestApplyRange(t *testing.T) {
	s := open(t, session(t))
	if _, err := s.Range(0, 10, 10); !errors.Is(err, ErrAfterNotIssued) { // 空の Store: 何も発行していない
		t.Fatalf("空の Store の Range: %v", err)
	}
	if err := s.Apply(Op{Append: rows(0, 10, 10)}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		after, before uint64
		limit         int
		want          []uint64
	}{
		{0, 5, 100, []uint64{1, 2, 3, 4}},
		{0, 100, 3, []uint64{1, 2, 3}},
		{8, 100, 100, []uint64{9}},
		{9, 100, 100, nil}, // 最後の seq (9) を持つ画面: 続きは無い (next=10 より小さい)
		{3, 4, 100, nil},
		{3, 5, 100, []uint64{4}},
		{3, 3, 100, nil},
		{0, 100, 0, nil},
		{0, ^uint64(0), 100, []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9}},
	}
	for _, c := range cases {
		got, err := s.Range(c.after, c.before, c.limit)
		if err != nil || !eq(seqs(got), c.want) {
			t.Errorf("Range(%d,%d,%d) = %v, %v (want %v)", c.after, c.before, c.limit, seqs(got), err, c.want)
		}
	}
	// after が、まだ書いていない seq: 不正 (S5)
	for _, a := range []uint64{10, 11, 1 << 40, ^uint64(0)} {
		if _, err := s.Range(a, ^uint64(0), 10); !errors.Is(err, ErrAfterNotIssued) {
			t.Errorf("Range(after=%d): %v", a, err)
		}
	}
	got, _ := s.Range(0, 3, 10)
	if len(got) != 2 || !bytes.Equal(got[0].Payload, rows(1, 2, 10)[0].Payload) {
		t.Errorf("payload が違う: %v", got)
	}
}

func TestApplyValidation(t *testing.T) {
	s := open(t, session(t))
	if err := s.Apply(Op{Append: rows(5, 8, 4)}); err != nil {
		t.Fatal(err)
	}
	bad := []Op{
		{Append: rows(7, 9, 4)}, // 増えていない
		{Append: []Row{{Seq: 9, Payload: []byte("x")}, {Seq: 9, Payload: []byte("y")}}},
		{Append: []Row{{Seq: 9}}}, // 空
		{Append: []Row{{Seq: 9, Payload: make([]byte, DefaultMaxPayload+1)}}},
		{Append: []Row{{Seq: 1 << 63, Payload: []byte("x")}}},
		{Pin: []uint64{1 << 63}},
	}
	for i, op := range bad {
		if err := s.Apply(op); err == nil {
			t.Errorf("不正な op %d が通った", i)
		}
	}
	// 失敗した op は、半端に書かない
	if got, _ := s.Range(4, 100, 100); !eq(seqs(got), []uint64{5, 6, 7}) {
		t.Errorf("失敗した op が、書いた: %v", seqs(got))
	}
	// 間が空く seq (durable でない Event) は、よい
	if err := s.Apply(Op{Append: rows(20, 22, 4)}); err != nil {
		t.Fatal(err)
	}
}

func TestFilesAreOwnerOnly(t *testing.T) {
	dir := session(t)
	old := umask(0)
	defer umask(old)
	s := open(t, dir)
	for i := 0; i < 50; i++ { // WAL・shm ができるまで書く
		if err := s.Apply(Op{Append: rows(uint64(i), uint64(i)+1, 3000)}); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{dbName, dbName + "-wal", dbName + "-shm", lockName}
	for _, n := range names {
		fi, err := os.Lstat(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("%s が無い: %v", n, err)
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v (0600 の通常のファイルでない)", n, fi.Mode())
		}
	}
}

func TestOpenRefusesWritableDir(t *testing.T) {
	d := session(t)
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(d, gen, Options{}); err == nil {
		t.Fatal("他の人に書けるディレクトリで、開けた")
	}
}

func TestOpenReplacesPreviousAndLeavesSymlinkTargets(t *testing.T) {
	dir := session(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 前の起動の残り (events.db・-wal) は消す。events.db が symlink でも、辿らず、リンクだけを消す
	if err := os.Symlink(victim, filepath.Join(dir, dbName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dbName+"-wal"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	if b, _ := os.ReadFile(victim); string(b) != "secret" {
		t.Fatalf("symlink の先が書き換わった: %q", b)
	}
	if err := s.Apply(Op{Append: rows(0, 3, 10)}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "secret" {
		t.Fatalf("symlink の先が書き換わった: %q", b)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, dbName)); !fi.Mode().IsRegular() {
		t.Fatalf("events.db が通常のファイルでない: %v", fi.Mode())
	}
}

func TestLockIsSymlinkSafe(t *testing.T) {
	dir := session(t)
	target := filepath.Join(t.TempDir(), "t")
	os.WriteFile(target, nil, 0o600)
	os.Symlink(target, filepath.Join(dir, lockName))
	if s, err := Open(dir, gen, Options{}); err == nil {
		s.Close(true)
		t.Fatal("events.lock が symlink なのに、開けた")
	}
}

func TestSecondOpenFailsWhileLocked(t *testing.T) {
	dir := session(t)
	s := open(t, dir)
	if _, err := Open(dir, gen, Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("2 つ目の Open: %v", err)
	}
	// 失敗した 2 つ目が、生きている DB を消していない
	if err := s.Apply(Op{Append: rows(0, 2, 10)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Range(0, 10, 10); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	s.Close(true)
	s2, err := Open(dir, gen, Options{}) // 閉じたら、開ける
	if err != nil {
		t.Fatal(err)
	}
	s2.Close(true)
}

func TestCloseRemove(t *testing.T) {
	dir := session(t)
	s, err := Open(dir, gen, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Apply(Op{Append: rows(0, 100, 3000)})
	if err := s.Close(true); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("残った: %v", ents)
	}
	if err := s.Close(true); err != nil { // 2 回目は何もしない
		t.Fatal(err)
	}
	if err := s.Apply(Op{Append: rows(200, 201, 1)}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.Range(0, 5, 5); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.Size(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestCloseKeepLeavesDB(t *testing.T) {
	dir := session(t)
	s, _ := Open(dir, gen, Options{})
	s.Apply(Op{Append: rows(0, 3, 10)})
	s.Close(false)
	if _, err := os.Lstat(filepath.Join(dir, dbName)); err != nil {
		t.Fatal(err)
	}
	// 閉じたあとは、ロックが取れる → 次の Open は、前の DB を消して、作り直す
	s2 := open(t, dir)
	if _, err := s2.Range(0, 10, 10); !errors.Is(err, ErrAfterNotIssued) {
		t.Fatalf("作り直していない: %v", err)
	}
}

func TestWeirdPaths(t *testing.T) {
	for _, name := range []string{"a?b", "a#b", "a%41b", "a b", "日本語", "a&b=c", "a?_pragma=query_only(1)", "100%", "a;b", "~x", "x:y", "file:z"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Skip(err)
			}
			s, err := Open(dir, gen, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Apply(Op{Append: rows(0, 3, 10)}); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Range(0, 10, 10); err != nil || len(got) != 2 {
				t.Fatalf("%v %v", got, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, dbName)); err != nil {
				t.Fatalf("DB が、期待した場所に無い: %v", err)
			}
			if err := s.Close(true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDSNEscapes(t *testing.T) {
	got := dsn("/a?b#c%d e", "x(1)")
	want := "file:///a%3Fb%23c%25d%20e?_pragma=x%281%29"
	if got != want {
		t.Fatalf("%q != %q", got, want)
	}
}

func TestTrim(t *testing.T) {
	s := open(t, session(t))
	if err := s.Apply(Op{Append: rows(0, 20, 100)}); err != nil { // 20 行 × 100 バイト
		t.Fatal(err)
	}
	if err := s.Apply(Op{Pin: []uint64{3, 7}}); err != nil {
		t.Fatal(err)
	}
	if first, ok := s.FirstUnpinned(); !ok || first != 0 {
		t.Fatalf("FirstUnpinned = %d %v", first, ok)
	}
	if first, err := s.Trim(2000); err != nil || first != 0 { // 予算内: 何も消さない
		t.Fatalf("Trim(予算内) = %d %v", first, err)
	}
	// 2000 → 1500: 固定でない行を古い順に 5 行 (0・1・2・4・5) 消す。3・7 は残る。
	first, err := s.Trim(1500)
	if err != nil || first != 6 {
		t.Fatalf("Trim = %d %v", first, err)
	}
	got, _ := s.Range(0, 100, 100)
	if !eq(seqs(got), []uint64{3, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}) {
		t.Fatalf("残り: %v", seqs(got))
	}
	// 固定が解けると、その行は、次の Trim で消える。連続の先頭は、消した範囲の次のまま
	if err := s.Apply(Op{Unpin: []uint64{3}}); err != nil {
		t.Fatal(err)
	}
	if first, _ := s.FirstUnpinned(); first != 6 {
		t.Fatalf("FirstUnpinned = %d (固定が解けた古い行で、先頭が戻った)", first)
	}
	if first, err := s.Trim(1000); err != nil || first != 11 { // 1500 → 1000: 3・6・8・9・10 (固定でない先頭 5 行 = 500 バイト)
		t.Fatalf("Trim = %d %v", first, err)
	}
	// 固定のものだけが残って、予算を超える場合: それ以上は消さずに止まる
	if _, err := s.Trim(0); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Range(0, 100, 100)
	if !eq(seqs(got), []uint64{7}) {
		t.Fatalf("固定の行だけが残るはず: %v", seqs(got))
	}
	if _, err := s.Trim(0); err != nil { // 何度呼んでも、止まる
		t.Fatal(err)
	}
	if got, _ := s.Range(0, 100, 100); !eq(seqs(got), []uint64{7}) {
		t.Fatalf("%v", seqs(got))
	}
	// 固定の行が残った後の追記・Range が、動く
	if err := s.Apply(Op{Append: rows(20, 23, 100)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Range(19, 100, 100); !eq(seqs(got), []uint64{20, 21, 22}) {
		t.Fatalf("%v", seqs(got))
	}
}

func TestPinOfMissingRowIsNoop(t *testing.T) {
	s := open(t, session(t))
	s.Apply(Op{Append: rows(0, 3, 10)})
	if err := s.Apply(Op{Pin: []uint64{99}, Unpin: []uint64{98}}); err != nil {
		t.Fatal(err)
	}
}

func TestSizeIncludesWAL(t *testing.T) {
	s := open(t, session(t))
	s.Apply(Op{Append: rows(0, 200, 2000)})
	n, err := s.Size()
	if err != nil || n < 200*2000 {
		t.Fatalf("Size = %d %v", n, err)
	}
}

func TestConcurrent(t *testing.T) {
	s := open(t, session(t))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(0); i < 300; i++ {
			op := Op{Append: rows(i, i+1, 200)}
			if i%10 == 0 {
				op.Pin = []uint64{i}
			}
			if i > 20 && i%10 == 5 {
				op.Unpin = []uint64{i - 5}
			}
			if err := s.Apply(op); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				got, err := s.Range(0, 1000, 50)
				if err != nil && !errors.Is(err, ErrAfterNotIssued) {
					t.Error(err)
					return
				}
				for j := 1; j < len(got); j++ {
					if got[j].Seq <= got[j-1].Seq {
						t.Errorf("seq 順でない")
					}
				}
				s.FirstUnpinned()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := s.Trim(20000); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
}

// ---- 異常終了の掃除 ----

func TestSweepStale(t *testing.T) {
	sessions := t.TempDir()
	mk := func(name string, files ...string) string {
		d := filepath.Join(sessions, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(d, f), bytes.Repeat([]byte{'x'}, 1000), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	stale := mk("stale", dbName, dbName+"-wal", dbName+"-shm", lockName) // 持ち主が死んだ残り
	staleNoLock := mk("stale-nolock", dbName)
	onlyLock := mk("only-lock", lockName)
	other := mk("other", "notes.txt") // events.* の無いディレクトリは、触らない
	plain := filepath.Join(sessions, "file")
	os.WriteFile(plain, []byte("x"), 0o600)
	victim := t.TempDir()
	os.WriteFile(filepath.Join(victim, dbName), []byte("v"), 0o600)
	os.Symlink(victim, filepath.Join(sessions, "link")) // symlink のディレクトリは、辿らない
	live := mk("live")
	s, err := Open(live, gen, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(true)
	s.Apply(Op{Append: rows(0, 50, 2000)})

	removed, total, err := SweepStale(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Errorf("removed = %d", removed)
	}
	for _, d := range []string{stale, staleNoLock, onlyLock} {
		if ents, _ := os.ReadDir(d); len(ents) != 0 {
			t.Errorf("%s に残った: %v", d, ents)
		}
	}
	if _, err := os.Stat(filepath.Join(other, "notes.txt")); err != nil {
		t.Error("関係ないファイルを消した")
	}
	if _, err := os.Stat(filepath.Join(victim, dbName)); err != nil {
		t.Error("symlink の先を消した")
	}
	if _, err := os.Stat(plain); err != nil {
		t.Error(err)
	}
	// 生きている DB は、消さない・使える・大きさを数える
	if got, err := s.Range(0, 100, 100); err != nil || len(got) != 49 {
		t.Fatalf("生きている DB が壊れた: %d %v", len(got), err)
	}
	if err := s.Apply(Op{Append: rows(50, 51, 10)}); err != nil {
		t.Fatal(err)
	}
	if want, _ := s.Size(); total < 50*2000 || total > want {
		t.Errorf("totalBytes = %d (Size %d)", total, want)
	}
	if _, err := os.Lstat(filepath.Join(live, lockName)); err != nil {
		t.Error("生きている DB のロックが消えた")
	}
	// 閉じた (ロックを手放した) 後なら、掃除できる
	s.Close(false)
	removed, _, err = SweepStale(sessions)
	if err != nil || removed != 1 {
		t.Errorf("閉じた後: %d %v", removed, err)
	}
	if removed, total, err := SweepStale(filepath.Join(sessions, "nonexistent")); removed != 0 || total != 0 || err != nil {
		t.Errorf("無いディレクトリ: %d %d %v", removed, total, err)
	}
}

// 掃除と起動が競合しても、生きている DB を消さない。
func TestSweepVsOpenRace(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "s")
	os.Mkdir(dir, 0o700)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				SweepStale(sessions)
			}
		}
	}()
	for i := 0; i < 40; i++ {
		s, err := Open(dir, gen, Options{})
		if errors.Is(err, ErrLocked) {
			continue // 掃除が持っている: 起動側は、縮退する (chat 側)
		}
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		for j := uint64(0); j < 5; j++ {
			if err := s.Apply(Op{Append: rows(j, j+1, 100)}); err != nil {
				t.Fatalf("掃除に、生きている DB を消された: %v", err)
			}
		}
		if got, err := s.Range(0, 10, 10); err != nil || len(got) != 4 {
			t.Fatalf("%v %v", got, err)
		}
		s.Close(i%2 == 0)
	}
	close(stop)
	wg.Wait()
}

// ---- 別のプロセスが DB を書き換えた場合 (DB の中身は信用しない) ----

func tamperDB(t *testing.T, s *Store) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(s.absDB, "busy_timeout(2000)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTamperedRows(t *testing.T) {
	s := open(t, session(t))
	s.Apply(Op{Append: rows(0, 5, 10)})
	db := tamperDB(t, s)
	// 値の型・範囲・大きさが想定外の行
	for _, q := range []string{
		`INSERT INTO events VALUES ('` + gen + `', 100, NULL, 0, 0)`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Fatalf("NOT NULL が効かない")
		}
	}
	db.Exec(`PRAGMA ignore_check_constraints=1`)
	db.Exec(`INSERT INTO events VALUES (?, 50, zeroblob(?), 0, 0)`, gen, DefaultMaxPayload+1) // 大きすぎる
	if _, err := s.Range(4, 1000, 100); err == nil {
		t.Fatal("大きすぎる行を、返した")
	}
	db.Exec(`DELETE FROM events WHERE seq = 50`)
	db.Exec(`INSERT INTO events VALUES (?, 60, '', 0, 0)`, gen) // 空
	if _, err := s.Range(4, 1000, 100); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("空の行: %v", err)
	}
	db.Exec(`DELETE FROM events WHERE seq = 60`)
	db.Exec(`INSERT INTO events VALUES (?, 'abc', x'00', 0, 0)`, gen)   // seq が数でない (列の型は INTEGER。文字列のまま入る)
	if got, err := s.Range(0, 1000, 100); err != nil || len(got) != 4 { // 文字列の seq は、数の範囲に入らない (SQLite は、文字列を数より大きく扱う): 返らない
		t.Fatalf("seq が数でない行: %v %v", seqs(got), err)
	}
	db.Exec(`DELETE FROM events WHERE seq = 'abc'`)
	db.Exec(`INSERT INTO events VALUES (?, -5, x'00', 0, 0)`, gen) // 負
	if got, err := s.Range(0, 1000, 100); err != nil {
		t.Logf("負の seq: %v", err)
	} else {
		for _, r := range got {
			if r.Seq > 1000 {
				t.Fatalf("範囲外: %d", r.Seq)
			}
		}
	}
	db.Exec(`DROP TABLE events`) // 表ごと消された
	if _, err := s.Range(0, 100, 10); err == nil {
		t.Fatal("表が無いのに、成功した")
	}
	if err := s.Apply(Op{Append: rows(10, 11, 5)}); err == nil {
		t.Fatal("表が無いのに、書けた")
	}
	s.FirstUnpinned()
	s.Trim(0)
	s.Size()
}

func TestCorruptedFileDoesNotPanic(t *testing.T) {
	for seed := 0; seed < 12; seed++ {
		dir := session(t)
		s, err := Open(dir, gen, Options{})
		if err != nil {
			t.Fatal(err)
		}
		s.Apply(Op{Append: rows(0, 300, 500)})
		// DB のファイルの途中を、壊す (checkpoint 済みの部分も、WAL の部分も)
		for _, name := range []string{dbName, dbName + "-wal"} {
			f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR, 0)
			if err != nil {
				continue
			}
			fi, _ := f.Stat()
			if fi.Size() > 0 {
				for k := 0; k < 8; k++ {
					off := (int64(seed)*7919 + int64(k)*104729) % fi.Size()
					f.WriteAt([]byte{byte(seed + k), 0xff, 0, 0x7f}, off)
				}
			}
			f.Close()
		}
		for i := 0; i < 3; i++ {
			s.Range(0, 1000, 100)
			s.FirstUnpinned()
			s.Apply(Op{Append: rows(uint64(300+i), uint64(301+i), 500), Pin: []uint64{uint64(i)}})
			s.Trim(1000)
		}
		s.Close(true)
	}
}

func FuzzRangeArgs(f *testing.F) {
	f.Add(uint64(0), uint64(10), 5)
	f.Add(^uint64(0), ^uint64(0), -1)
	f.Add(uint64(1<<63), uint64(0), 1<<30)
	dir := filepath.Join(f.TempDir(), "s")
	os.Mkdir(dir, 0o700)
	s, err := Open(dir, gen, Options{})
	if err != nil {
		f.Fatal(err)
	}
	defer s.Close(true)
	s.Apply(Op{Append: rows(0, 20, 10)})
	f.Fuzz(func(t *testing.T, after, before uint64, limit int) {
		got, err := s.Range(after, before, limit)
		if err != nil && !errors.Is(err, ErrAfterNotIssued) {
			t.Fatal(err)
		}
		if after >= 20 && err == nil {
			t.Fatalf("未発行の after=%d が通った", after)
		}
		prev := after
		for _, r := range got {
			if r.Seq <= prev || r.Seq >= before {
				t.Fatalf("範囲外 %d (after=%d before=%d)", r.Seq, after, before)
			}
			prev = r.Seq
		}
		if len(got) > maxRangeRows {
			t.Fatal("件数の上限を超えた")
		}
	})
}

func TestRangeByteCap(t *testing.T) {
	s := open(t, session(t))
	for i := uint64(0); i < 6; i++ {
		if err := s.Apply(Op{Append: []Row{{Seq: i, Payload: bytes.Repeat([]byte{'x'}, 3<<20)}}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Range(0, 100, 100)
	if err != nil || len(got) < 1 || len(got) > 3 {
		t.Fatalf("バイトの上限で切る: %d 件 %v", len(got), err)
	}
	// 続きを、最後の seq から読める
	more, err := s.Range(got[len(got)-1].Seq, 100, 100)
	if err != nil || len(more) == 0 {
		t.Fatalf("続き: %v %v", more, err)
	}
}

func TestGenerationValidation(t *testing.T) {
	dir := session(t)
	for _, g := range []string{"", "a b", "a\nb", string(make([]byte, 65)), "日本"} {
		if s, err := Open(dir, g, Options{}); err == nil {
			s.Close(true)
			t.Errorf("generation %q が通った", g)
		}
	}
	// 失敗した Open は、何も残さない
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("残った: %v", ents)
	}
}

var _ = fmt.Sprint

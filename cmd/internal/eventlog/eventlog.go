package eventlog

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // ADR 0026: この package だけ
)

// 量の既定。
const (
	DefaultMaxPayload = 4 << 20 // 1 行の payload の上限 (chat の DefaultMaxEvent は 2 MiB)
	maxGeneration     = 64      // generation の長さの上限
	maxRangeRows      = 4096    // Range の 1 回の件数の上限
	maxRangeBytes     = 8 << 20 // Range の 1 回の合計バイトの上限 (最低 1 件は返す)
	readConns         = 4       // 読み取りの同時接続
	schemaVersion     = 1       // PRAGMA user_version
)

var (
	// ErrClosed は、Close のあとの操作。
	ErrClosed = errors.New("eventlog: 閉じている")
	// ErrLocked は、セッションのディレクトリの flock を、他 (別の serve・掃除) が持っている。
	ErrLocked = errLocked
	// ErrAfterNotIssued は、Range の after が、まだ書いていない seq (next 以上) を指す (不正。呼び手は、全再送に倒す。ADR 0024 決定 1)。
	ErrAfterNotIssued = errors.New("eventlog: after が、まだ書いていない seq を指す")
	// ErrSeqOrder は、Apply の追記の seq が、増えていない・範囲外。
	ErrSeqOrder = errors.New("eventlog: 追記の seq が増えていない・範囲外")
	// ErrPayload は、Apply の payload が、空・上限超え。
	ErrPayload = errors.New("eventlog: payload が空・大きすぎる")
	// ErrCorrupt は、DB の中身が想定外 (別のプロセスが書き換えた・壊れた)。
	ErrCorrupt = errors.New("eventlog: DB の中身が想定外")
)

// Row は、1 件の Event。Payload は Event.JSON そのもの。
type Row struct {
	Seq     uint64
	Payload []byte
}

// Op は、1 回の書き込み (1 トランザクション)。追記 → 固定 → 固定の解除、の順に適用する。
// Pin・Unpin の対象が (GC で) 無ければ、何もしない。
type Op struct {
	Append     []Row
	Pin, Unpin []uint64
}

// Options は、Open の設定。ゼロ値で既定。
type Options struct {
	MaxPayload int // 1 行の payload の上限 (既定 DefaultMaxPayload)
}

// Store は、1 セッション・1 起動 (generation) の耐久ログ。複数の goroutine から呼んでよい (書き込みは直列)。
type Store struct {
	gen        string
	maxPayload int
	absDB      string

	root  *os.Root
	lock  *os.File
	w, r  *sql.DB
	dbInf os.FileInfo // 作った DB の stat (開いたあとの検査用)

	writeMu sync.Mutex // Apply・Trim・Close を直列にする
	total   int64      // 書いた payload の合計 (固定の行を含む。writeMu の中だけ)。Trim が、DB を全部は数えずに済む
	mu      sync.Mutex // 以下の状態
	closed  bool
	hasLast bool
	last    uint64 // 最後に書いた seq
	hasTrim bool
	trimmed uint64 // Trim が消した範囲の上端 (これ以下の、固定されていない行は無い)
}

// Open は、dir (セッションのディレクトリ) の flock を取って、前の DB を消し、新しい DB を作る。
// generation は、この起動の世代 (1〜64 バイト。表示できる ASCII)。ロックを他が持っていれば ErrLocked。
func Open(dir, generation string, opts Options) (*Store, error) {
	if !validGeneration(generation) {
		return nil, errors.New("eventlog: generation が不正")
	}
	if opts.MaxPayload <= 0 {
		opts.MaxPayload = DefaultMaxPayload
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if strings.ContainsRune(abs, 0) {
		return nil, errors.New("eventlog: path に NUL がある")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	if err := checkDir(root); err != nil {
		root.Close()
		return nil, err
	}
	lock, err := acquireLock(root)
	if err != nil {
		root.Close()
		return nil, err
	}
	s := &Store{gen: generation, maxPayload: opts.MaxPayload, absDB: filepath.Join(abs, dbName), root: root, lock: lock}
	for attempt := 0; ; attempt++ {
		err = s.create()
		if err == nil {
			return s, nil
		}
		s.closeDBs()
		removeDBFiles(root) // 自分のセッションのファイルだけ
		if attempt >= 1 || !errors.Is(err, ErrCorrupt) {
			lock.Close()
			root.Close()
			return nil, err
		}
	}
}

func validGeneration(g string) bool {
	if g == "" || len(g) > maxGeneration {
		return false
	}
	for i := 0; i < len(g); i++ {
		if g[i] < 0x21 || g[i] > 0x7e {
			return false
		}
	}
	return true
}

// create は、前の DB を消し、DB を先に作って (O_EXCL・0600)、SQLite に開かせ、スキーマを作る。
func (s *Store) create() error {
	if err := removeDBFiles(s.root); err != nil {
		return err
	}
	f, err := s.root.OpenFile(dbName, os.O_RDWR|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	if err := checkFile(fi); err != nil {
		return err
	}
	s.dbInf = fi

	w, err := sql.Open("sqlite", dsn(s.absDB, "page_size(16384)", "busy_timeout(50)", "synchronous(NORMAL)", "secure_delete(ON)",
		"temp_store(MEMORY)", "trusted_schema(OFF)", "cell_size_check(ON)", "journal_size_limit(8388608)"))
	if err != nil {
		return err
	}
	s.w = w
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	// 新しいはずの DB に、何かある (開く前に、別のプロセスが書いた) なら、捨てて作り直す。
	var tables, ver int
	if err := w.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&tables); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err := w.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if tables != 0 || ver != 0 {
		return fmt.Errorf("%w: 作ったばかりの DB が空でない", ErrCorrupt)
	}
	if _, err := w.Exec(`CREATE TABLE events (
		generation TEXT NOT NULL,
		seq INTEGER NOT NULL CHECK (seq >= 0),
		payload BLOB NOT NULL,
		pinned INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0, 1)),
		created_at INTEGER NOT NULL,
		PRIMARY KEY (generation, seq))`); err != nil {
		return err
	}
	if _, err := w.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	// WAL は、表を作ったあとに切り替える (page_size を、DSN の pragma と WAL の切り替えを同時にすると、4096 のままになる)。
	var mode string
	if err := w.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("eventlog: WAL にできない (%q)", mode)
	}

	r, err := sql.Open("sqlite", dsn(s.absDB, "busy_timeout(50)", "query_only(1)", "temp_store(MEMORY)", "trusted_schema(OFF)", "cell_size_check(ON)"))
	if err != nil {
		return err
	}
	s.r = r
	r.SetMaxOpenConns(readConns)
	r.SetMaxIdleConns(readConns)
	if err := r.Ping(); err != nil {
		return err
	}
	return s.verifyFiles()
}

// verifyFiles は、SQLite が path で開いたファイルが、作ったものと同じで、権限が 0600 であることを確かめる (-wal・-shm も)。
func (s *Store) verifyFiles() error {
	cur, err := os.Lstat(s.absDB)
	if err != nil {
		return err
	}
	if !os.SameFile(s.dbInf, cur) {
		return fmt.Errorf("%w: %s が、作ったファイルと違う", ErrCorrupt, dbName)
	}
	if err := checkFile(cur); err != nil {
		return err
	}
	for _, n := range []string{dbName + "-wal", dbName + "-shm"} {
		fi, err := s.root.Lstat(n)
		if err != nil {
			continue // まだ無い (WAL は、最初の書き込みで作られる)
		}
		if err := checkFile(fi); err != nil {
			return err
		}
	}
	return nil
}

// dsn は、path (絶対) を URI としてエスケープし、pragma を付けた DSN を作る。
// modernc は '?' で DSN を切り、'#'・'%' は URI で特別なので、path は全てエスケープする。
func dsn(path string, pragmas ...string) string {
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '/', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	for i, p := range pragmas {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString("_pragma=" + url.QueryEscape(p))
	}
	return b.String()
}

func (s *Store) closeDBs() {
	if s.r != nil {
		s.r.Close()
		s.r = nil
	}
	if s.w != nil {
		s.w.Close()
		s.w = nil
	}
}

func (s *Store) state() (next uint64, hasLast, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasLast {
		return s.last + 1, true, s.closed
	}
	return 0, false, s.closed
}

// Apply は、op を 1 トランザクションで書く。
func (s *Store) Apply(op Op) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	closed, hasLast, last := s.closed, s.hasLast, s.last
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	// 検査は、書く前に全部行う (途中で失敗して、半端に書かない)。
	prev, have := last, hasLast
	for _, row := range op.Append {
		if len(row.Payload) == 0 || len(row.Payload) > s.maxPayload {
			return ErrPayload
		}
		if row.Seq >= math.MaxInt64 || (have && row.Seq <= prev) {
			return ErrSeqOrder
		}
		prev, have = row.Seq, true
	}
	for _, q := range [][]uint64{op.Pin, op.Unpin} {
		for _, seq := range q {
			if seq >= math.MaxInt64 {
				return ErrSeqOrder
			}
		}
	}
	if len(op.Append) == 0 && len(op.Pin) == 0 && len(op.Unpin) == 0 {
		return nil
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if len(op.Append) > 0 {
		st, err := tx.Prepare(`INSERT INTO events (generation, seq, payload, pinned, created_at) VALUES (?, ?, ?, 0, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, row := range op.Append {
			if _, err := st.Exec(s.gen, int64(row.Seq), row.Payload, now); err != nil {
				return err
			}
		}
	}
	for _, p := range []struct {
		seqs []uint64
		val  int
	}{{op.Pin, 1}, {op.Unpin, 0}} {
		if len(p.seqs) == 0 {
			continue
		}
		st, err := tx.Prepare(`UPDATE events SET pinned = ? WHERE generation = ? AND seq = ?`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, seq := range p.seqs {
			if _, err := st.Exec(p.val, s.gen, int64(seq)); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, row := range op.Append {
		s.total += int64(len(row.Payload))
	}
	if prev != last || have != hasLast {
		s.mu.Lock()
		s.last, s.hasLast = prev, have
		s.mu.Unlock()
	}
	return nil
}

// Range は、seq が (after, before) の範囲 (両端を含まない) の行を、seq 順に返す (固定の行を含む)。
// 件数 (limit・最大 4096) と合計バイト (約 8 MiB・最低 1 件) で切るので、返った行が少なくても、続きがあるかもしれない
// (最後の seq を after にして、また呼ぶ)。after が、まだ書いていない seq (next 以上) なら ErrAfterNotIssued (ADR 0024 決定 1)。
// 返す Payload は、検証されていない (別のプロセスが DB を書き換えうる): 呼び手が、配る前に検証する。
func (s *Store) Range(after, before uint64, limit int) ([]Row, error) {
	next, _, closed := s.state()
	if closed {
		return nil, ErrClosed
	}
	if after >= next {
		return nil, ErrAfterNotIssued
	}
	if limit <= 0 || before <= after+1 {
		return nil, nil
	}
	if limit > maxRangeRows {
		limit = maxRangeRows
	}
	if before > math.MaxInt64 {
		before = math.MaxInt64
	}
	rows, err := s.r.Query(`SELECT seq, CASE WHEN length(CAST(payload AS BLOB)) > ? THEN NULL ELSE payload END FROM events
		WHERE generation = ? AND seq > ? AND seq < ? ORDER BY seq LIMIT ?`, s.maxPayload, s.gen, int64(after), int64(before), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	total := 0
	prev := after
	for rows.Next() {
		var seq int64
		var payload []byte
		if err := rows.Scan(&seq, &payload); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if seq < 0 || uint64(seq) <= prev || uint64(seq) >= before || len(payload) == 0 {
			return nil, ErrCorrupt
		}
		prev = uint64(seq)
		out = append(out, Row{Seq: prev, Payload: payload})
		total += len(payload)
		if total >= maxRangeBytes {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FirstUnpinned は、hello の first_seq 用: 連続して残っている先頭の seq。Trim が消した範囲より後で、最小の seq (まだ Trim していなければ、
// 最小の seq)。それ以前に残る固定の行は、固定が解けるまで、連続の外 (Range は返す)。行が無ければ false (エラーも false)。
func (s *Store) FirstUnpinned() (uint64, bool) {
	_, _, closed := s.state()
	if closed {
		return 0, false
	}
	s.mu.Lock()
	hasTrim, trimmed := s.hasTrim, s.trimmed
	s.mu.Unlock()
	var min sql.NullInt64
	var err error
	if hasTrim {
		err = s.r.QueryRow(`SELECT min(seq) FROM events WHERE generation = ? AND seq > ?`, s.gen, int64(trimmed)).Scan(&min)
	} else {
		err = s.r.QueryRow(`SELECT min(seq) FROM events WHERE generation = ?`, s.gen).Scan(&min)
	}
	if err != nil || !min.Valid || min.Int64 < 0 {
		return 0, false
	}
	return uint64(min.Int64), true
}

// Trim は、payload の合計 (固定の行を含む) が budgetBytes に収まるまで、固定されていない行を、古い順に消す。固定の行は消さない:
// 固定の行だけが残って予算を超えるときは、そこで止まる (無限に回らない)。返す firstSeq は、FirstUnpinned と同じ。
func (s *Store) Trim(budgetBytes int64) (uint64, error) {
	s.writeMu.Lock()
	err := s.trim(budgetBytes)
	s.writeMu.Unlock()
	if err != nil {
		return 0, err
	}
	first, _ := s.FirstUnpinned()
	return first, nil
}

func (s *Store) trim(budget int64) error {
	_, _, closed := s.state()
	if closed {
		return ErrClosed
	}
	if budget < 0 {
		budget = 0
	}
	if s.total <= budget {
		return nil
	}
	need := s.total - budget
	// 固定されていない行を、古い順に、必要な分に届くまで読む (全部は読まない)。その最後の行まで消す。
	rows, err := s.w.Query(`SELECT seq, length(CAST(payload AS BLOB)) FROM events WHERE generation = ? AND pinned = 0 ORDER BY seq`, s.gen)
	if err != nil {
		return err
	}
	var cutoff, freed int64
	found := false
	for rows.Next() {
		var seq, n int64
		if err := rows.Scan(&seq, &n); err != nil {
			rows.Close()
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if seq < 0 || n < 0 {
			rows.Close()
			return ErrCorrupt
		}
		cutoff, freed, found = seq, freed+n, true
		if freed >= need {
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !found { // 固定されていない行が無い (固定の行だけが残って、予算を超える): 止まる
		return nil
	}
	if _, err := s.w.Exec(`DELETE FROM events WHERE generation = ? AND pinned = 0 AND seq <= ?`, s.gen, cutoff); err != nil {
		return err
	}
	s.total -= freed
	s.mu.Lock()
	if !s.hasTrim || uint64(cutoff) > s.trimmed {
		s.trimmed, s.hasTrim = uint64(cutoff), true
	}
	s.mu.Unlock()
	return nil
}

// Size は、DB・-wal・-shm のファイルの大きさの合計。
func (s *Store) Size() (int64, error) {
	_, _, closed := s.state()
	if closed {
		return 0, ErrClosed
	}
	return dirSize(s.root), nil
}

// dirSize は、root の DB・-wal・-shm の大きさの合計 (無いものは 0)。
func dirSize(root *os.Root) int64 {
	var n int64
	for _, name := range dbFiles {
		if fi, err := root.Lstat(name); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n
}

// Close は、DB を閉じる。remove なら、DB・-wal・-shm を消す (ロックを持ったまま消し、最後にロックを手放す)。
// 2 回目以降は何もしない。
func (s *Store) Close(remove bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if s.w != nil {
		errs = append(errs, s.w.Close())
	}
	if remove {
		errs = append(errs, removeDBFiles(s.root))
	}
	errs = append(errs, s.lock.Close(), s.root.Close())
	return errors.Join(errs...)
}

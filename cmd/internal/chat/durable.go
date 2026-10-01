package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 耐久イベントログの chat 側 (ADR 0023・0024・0025・0027・0053): Hub.Update は、Mutex の中で、Hub が実際に決めた結果を有界のキューに積むだけ
// (待たない・O(1))。専用の 1 つの goroutine (storeWriter) が、まとめて EventStore.Apply し、書き込み済みの最大の seq (W) と縮退の判定を持つ。
// SubscribeAfter は、DB (リングより前)・リング・ライブの順に、取りこぼしも重複も無く続ける。I/O は EventStore の実装 (cmd/internal/eventlog) が持つ。

// StoredEvent は、EventStore の 1 行。Payload は Event.JSON そのもの。
type StoredEvent struct {
	Seq     uint64
	Payload []byte
}

// StoreOp は、EventStore.Apply の 1 回 (1 トランザクション)。Append → Pin → Unpin の順に適用する。Pin と Unpin は互いに重ならない
// (書き手が、同じ seq の最後の状態だけを出す)。Pin・Unpin は、既にある行 (この Apply で Append した行を含む) の固定の印だけを変える (行が無ければ何もしない)。
type StoreOp struct {
	Append     []StoredEvent
	Pin, Unpin []uint64
}

// EventStore は、耐久イベントログの Store (同期。cmd/internal/eventlog の *Store が満たす形。型の変換は cmd/goronation の配線が持つ)。
// 並行に呼ばれる: Apply・Trim は書き手の goroutine 1 本から、Range・FirstUnpinned は購読ごとの goroutine から。
// Store から読んだ行は信用しない (同じ利用者の別プロセスが、DB を書き換えうる)。Hub が検査してから配る (Backfill)。
type EventStore interface {
	// Apply は、op を 1 トランザクションで適用する。error なら何も適用していない。
	Apply(op StoreOp) error
	// Range は、after < seq < before の行を、seq の昇順に、最大 limit 件返す (固定の行を含む)。limit 件に満たないのは、尽きたとき。
	Range(after, before uint64, limit int) ([]StoredEvent, error)
	// FirstUnpinned は、固定でない行のうち、最小の seq (無ければ ok=false)。Trim のあとの hello の first_seq に使う。
	FirstUnpinned() (seq uint64, ok bool)
	// Trim は、固定でない行を古い方から削り、payload の合計を budgetBytes に収め、残った固定でない行の最小の seq を返す。固定の行は消さない。
	Trim(budgetBytes int64) (firstSeq uint64, err error)
}

// 書き手の既定 (HubConfig の 0 は既定)。
const (
	DefaultStoreQueueBytes   = 4 << 20  // キューの上限 (ADR 0027 決定 4。リングの既定と同じ)
	DefaultStoreMaxBytes     = 64 << 20 // 起動あたりの payload の合計の上限 (ADR 0027 決定 6)
	defaultBackfillBatch     = 64       // Backfill の 1 回の Range の件数
	writeBatchEvents         = 256      // 1 回の Apply にまとめる Event の上限 (件数)
	writeBatchBytes          = 1 << 20  // 同 (バイト)
	defaultSlowCommit        = 50 * time.Millisecond
	defaultHardCommit        = time.Second
	defaultSlowRun           = 5
	gcTargetNum, gcTargetDen = 3, 4 // 上限を超えたら、上限の 3/4 まで削る (毎回の Trim を避ける)
)

// ErrBadAfter は、SubscribeAfter の after が、まだ発行していない seq を指している (nextSeq 以上。呼び手は全再送に倒す)。
var ErrBadAfter = errors.New("chat: after が、発行していない seq を指している")

// ErrBackfillStale は、Backfill を読んでいる最中に GC (Trim) が走った。縮退ではない: 呼び手は接続を閉じ、画面は進んだ after で繋ぎ直す。
var ErrBackfillStale = errors.New("chat: Backfill の最中に、古い行を削った (繋ぎ直す)")

// ErrBackfillRead は、Backfill が Store から読めなかった。Store の error (path を含みうる) は、呼び手に返さない (OnDegrade の reason にだけ入る)。
var ErrBackfillRead = errors.New("chat: Store から読めなかった (耐久化を止めた)")

// ErrBadStoredEvent は、Store から読んだ行が、検査を通らない (改ざん・破損)。Hub は耐久化を止める (縮退)。
var ErrBadStoredEvent = errors.New("chat: Store の行が不正")

// storeWriter は、Hub の耐久ログの書き手。
type storeWriter struct {
	store      EventStore
	onDegrade  func(reason string)
	queueMax   int
	gcMax      int64
	slowCommit time.Duration
	hardCommit time.Duration
	slowRun    int

	written  atomic.Int64 // W: 書き込み済みの最大の seq (無ければ -1)
	degraded atomic.Bool
	trims    atomic.Int64 // Trim の前後で 1 ずつ増える (奇数は Trim の最中)。Backfill が、読んでいる最中の GC を検出する

	mu      sync.Mutex // 小さな Mutex (Hub.mu の中から取る。順序: Hub.mu → mu)。キューの出し入れだけ
	pending []StoreOp
	bytes   int
	closing bool
	notify  chan struct{}
	done    chan struct{}
}

func newStoreWriter(cfg HubConfig) *storeWriter {
	w := &storeWriter{
		store: cfg.Store, onDegrade: cfg.OnDegrade, queueMax: cfg.StoreQueueBytes, gcMax: cfg.StoreMaxBytes,
		slowCommit: cfg.slowCommit, hardCommit: cfg.hardCommit, slowRun: cfg.slowRun,
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if w.queueMax <= 0 {
		w.queueMax = DefaultStoreQueueBytes
	}
	if w.gcMax <= 0 {
		w.gcMax = DefaultStoreMaxBytes
	}
	if w.slowCommit <= 0 {
		w.slowCommit = defaultSlowCommit
	}
	if w.hardCommit <= 0 {
		w.hardCommit = defaultHardCommit
	}
	if w.slowRun <= 0 {
		w.slowRun = defaultSlowRun
	}
	w.written.Store(-1)
	go w.run()
	return w
}

// degrade は、耐久化を止める (以後、書かない・SubscribeAfter は全再送)。最初の 1 回だけ onDegrade を呼ぶ (Mutex の外の goroutine で。呼び手のコールバックが、
// Hub の Mutex を待たせない)。どの Mutex の中からでも呼べる。
func (w *storeWriter) degrade(reason string) {
	if !w.degraded.CompareAndSwap(false, true) {
		return
	}
	w.mu.Lock()
	w.pending, w.bytes = nil, 0
	w.mu.Unlock()
	w.wake()
	if w.onDegrade != nil {
		go w.onDegrade(reason)
	}
}

func (w *storeWriter) wake() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// enqueue は、Hub.Update の Mutex の中から呼ぶ: キューに積むだけ (O(1) ならし)。溢れたら縮退する。
func (w *storeWriter) enqueue(op StoreOp) {
	if w.degraded.Load() || (len(op.Append) == 0 && len(op.Pin) == 0 && len(op.Unpin) == 0) {
		return
	}
	size := opSize(op)
	w.mu.Lock()
	if w.closing || w.degraded.Load() {
		w.mu.Unlock()
		return
	}
	if w.bytes+size > w.queueMax {
		w.mu.Unlock()
		w.degrade("書き込みのキューが溢れた")
		return
	}
	w.pending = append(w.pending, op)
	w.bytes += size
	w.mu.Unlock()
	w.wake()
}

// opSize は、キューの勘定に使う op の大きさ (追記は Event の大きさ・固定の操作は 1 件ごとの固定の分。固定の操作だけの洪水も有界にする)。
func opSize(op StoreOp) int {
	n := 64 + 16*(len(op.Pin)+len(op.Unpin))
	for _, e := range op.Append {
		n += len(e.Payload) + 64
	}
	return n
}

// closeInput は、これ以上積まない (Hub.Close)。キューに残っている分は、書き切ってから goroutine が終わる。
func (w *storeWriter) closeInput() {
	w.mu.Lock()
	w.closing = true
	w.mu.Unlock()
	w.wake()
}

// take は、キューから、1 回の Apply 分 (件数・バイトの上限まで。少なくとも 1 つの op) を取り出し、同じ seq の固定の操作を最後の状態にまとめる。
// 空なら待つ。入力が閉じて空なら ok=false。
func (w *storeWriter) take() (op StoreOp, ok bool) {
	for {
		w.mu.Lock()
		if w.degraded.Load() {
			w.mu.Unlock()
			return StoreOp{}, false
		}
		if len(w.pending) > 0 {
			var ops []StoreOp
			n, b := 0, 0
			for len(w.pending) > 0 && (len(ops) == 0 || (n < writeBatchEvents && b < writeBatchBytes)) {
				p := w.pending[0]
				w.pending[0] = StoreOp{}
				w.pending = w.pending[1:]
				ops = append(ops, p)
				n += len(p.Append)
				b += opSize(p)
			}
			if len(w.pending) == 0 {
				w.pending = nil
			}
			w.bytes -= b
			w.mu.Unlock()
			return mergeOps(ops), true
		}
		if w.closing {
			w.mu.Unlock()
			return StoreOp{}, false
		}
		w.mu.Unlock()
		<-w.notify
	}
}

// mergeOps は、ops を順に 1 つにする。追記は連結、固定・解除は、seq ごとの最後の状態 (Pin と Unpin は重ならない)。
func mergeOps(ops []StoreOp) StoreOp {
	if len(ops) == 1 {
		return normalizePins(ops[0])
	}
	var out StoreOp
	state := map[uint64]bool{}
	var order []uint64
	for _, o := range ops {
		out.Append = append(out.Append, o.Append...)
		for _, s := range o.Pin {
			if _, seen := state[s]; !seen {
				order = append(order, s)
			}
			state[s] = true
		}
		for _, s := range o.Unpin {
			if _, seen := state[s]; !seen {
				order = append(order, s)
			}
			state[s] = false
		}
	}
	for _, s := range order {
		if state[s] {
			out.Pin = append(out.Pin, s)
		} else {
			out.Unpin = append(out.Unpin, s)
		}
	}
	return out
}

// normalizePins は、1 つの op の中の Pin と Unpin の重なりを、後の状態 (Unpin の後に Pin なら固定) にそろえる。Hub.Update は、pin → unpin の順に
// 適用する (Unpin が勝つ) ので、重なったら Unpin に倒す。
func normalizePins(o StoreOp) StoreOp {
	if len(o.Pin) == 0 || len(o.Unpin) == 0 {
		return o
	}
	un := make(map[uint64]struct{}, len(o.Unpin))
	for _, s := range o.Unpin {
		un[s] = struct{}{}
	}
	pin := o.Pin[:0:0]
	for _, s := range o.Pin {
		if _, both := un[s]; !both {
			pin = append(pin, s)
		}
	}
	o.Pin = pin
	return o
}

// run は、書き手の goroutine: take → Apply → W の更新・縮退の判定 → 上限を超えたら Trim。
func (w *storeWriter) run() {
	defer close(w.done)
	var stored int64 // これまでに書いた payload の合計 (Trim のあとは、目標の大きさ)
	slowRun := 0
	for {
		op, ok := w.take()
		if !ok {
			return
		}
		start := time.Now()
		err := w.store.Apply(op)
		d := time.Since(start)
		if w.degraded.Load() {
			return
		}
		if err != nil {
			w.degrade("書き込みの失敗: " + err.Error())
			return
		}
		if n := len(op.Append); n > 0 {
			w.written.Store(int64(op.Append[n-1].Seq))
			for _, e := range op.Append {
				stored += int64(len(e.Payload))
			}
		}
		if !w.judge(d, &slowRun) {
			return
		}
		if stored > w.gcMax {
			target := w.gcMax / gcTargetDen * gcTargetNum
			start = time.Now()
			w.trims.Add(1)
			_, err := w.store.Trim(target)
			w.trims.Add(1)
			d = time.Since(start)
			if err != nil {
				w.degrade("古い行の削除の失敗: " + err.Error())
				return
			}
			stored = target
			if !w.judge(d, &slowRun) {
				return
			}
		}
	}
}

// judge は、1 回の書き込みの時間 d を判定する (ADR 0027 決定 5): hardCommit 超ならその場で縮退・slowCommit 超が slowRun 回続いたら縮退。縮退したら false。
func (w *storeWriter) judge(d time.Duration, slowRun *int) bool {
	switch {
	case d > w.hardCommit:
		w.degrade(fmt.Sprintf("1 回の書き込みが %v かかった", d.Round(time.Millisecond)))
		return false
	case d > w.slowCommit:
		*slowRun++
		if *slowRun >= w.slowRun {
			w.degrade(fmt.Sprintf("%v を超える書き込みが %d 回続いた", w.slowCommit, *slowRun))
			return false
		}
	default:
		*slowRun = 0
	}
	return true
}

// Durable は、耐久ログが有効で、縮退していないか。
func (h *Hub) Durable() bool { return h.w != nil && !h.w.degraded.Load() }

// WaitStore は、Close のあと、キューに残った分を書き終える (または縮退して止まる) まで待つ。ctx が先に終わったらその error。
// 呼び手 (serve) は、これのあとに Store を閉じる (書き手が閉じた Store に書かない)。Store が無ければ、すぐ戻る。
func (h *Hub) WaitStore(ctx context.Context) error {
	if h.w == nil {
		return nil
	}
	select {
	case <-h.w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubscribeAfter は、seq が after より後の Event から、購読を始める (ADR 0024)。
//
// after が、まだ発行していない seq (nextSeq 以上) なら ErrBadAfter (呼び手は Subscribe で全再送に倒す)。
// 耐久ログが無い・縮退している・DB に無い固定の Event が範囲にある、のときは、Subscribe と同じ全再送で、Resumed() が false。
// そうでなければ、Resumed() が true で、リングより前の分 (after < seq < リングの先頭) を Backfill が DB から返し、Snapshot にはリングの
// seq > after の分が入る。Backfill → Snapshot → ライブの順に、取りこぼしも重複も無い (リングの先頭の決定と登録が、同じ Mutex の中)。
// 終了した Hub でも動く。
func (h *Hub) SubscribeAfter(after uint64) (*Subscription, error) {
	var trims int64
	var firstUnpinned uint64
	var haveFirst bool
	if h.w != nil {
		trims = h.w.trims.Load() // この値が Backfill の最中に変わったら (GC が走った)、Backfill は失敗する (ErrBackfillStale)
		if trims > 0 && !h.w.degraded.Load() {
			firstUnpinned, haveFirst = h.w.store.FirstUnpinned() // Mutex の外 (Store の I/O を Mutex の中に置かない)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if after >= h.nextSeq {
		return nil, ErrBadAfter
	}
	if h.w == nil || h.w.degraded.Load() || trims&1 == 1 || h.pinnedOnlyInMemory(after) { // Trim の最中は、first_seq を決められない: 全再送
		return h.subscribeLocked()
	}
	if len(h.subs) >= hubSubscribersHardLimit {
		return nil, ErrTooManySubscribers
	}
	ring := h.ring[h.head:]
	ringFirst := h.nextSeq
	if len(ring) > 0 {
		ringFirst = ring[0].Seq
	}
	s := &Subscription{hub: h, notify: make(chan struct{}, 1), resumed: true, durable: true}
	// 削った行があるときだけ、first_seq は DB の先頭 (固定でない行が無ければ、リングの先頭): UI が、after より前の省略を示せる。削っていなければ、省略は無い (0)。
	if trims > 0 {
		s.FirstSeq = ringFirst
		if haveFirst {
			s.FirstSeq = min(firstUnpinned, ringFirst)
		}
	}
	i := sortSearchSeq(ring, after)
	s.Snapshot = append([]Event(nil), ring[i:]...)
	if after+1 < ringFirst { // after+1 == ringFirst なら、DB に読む分は無い
		s.Backfill = &Backfill{h: h, store: h.w.store, cursor: after, before: ringFirst, batch: defaultBackfillBatch, trims: trims}
	}
	if h.ended {
		s.done, s.exit = true, h.exitCode
		return s, nil
	}
	h.subs[s] = struct{}{}
	return s, nil
}

// sortSearchSeq は、ring (seq の昇順) のうち、seq > after の最初の添字。
func sortSearchSeq(ring []Event, after uint64) int {
	lo, hi := 0, len(ring)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if ring[m].Seq <= after {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

// pinnedOnlyInMemory は、after より後で、リングより前の固定の Event のうち、DB に無いもの (durable でない) があるか。あれば DB からの続きでは届かない。
func (h *Hub) pinnedOnlyInMemory(after uint64) bool {
	ringFirst := h.nextSeq
	if r := h.ring[h.head:]; len(r) > 0 {
		ringFirst = r[0].Seq
	}
	for _, e := range h.pinned {
		if e.Seq > after && e.Seq < ringFirst && !e.Durable {
			return true
		}
	}
	return false
}

// Backfill は、SubscribeAfter が返す、DB の続き (リングより前の分)。Subscription の Snapshot を送る前に、Next が io.EOF を返すまで読んで送る。
// 並行には呼べない。読んだ行は、検査 (storedEvent) を通ったものだけを返す。Store の error・不正な行は、Hub を縮退させて返す (次の SubscribeAfter は全再送)。
type Backfill struct {
	h      *Hub
	store  EventStore
	cursor uint64 // これまでに返した最大の seq (最初は after)
	before uint64 // この seq より前だけ読む (SubscribeAfter の時点のリングの先頭)
	batch  int
	trims  int64 // SubscribeAfter の時点の GC の回数
	done   bool
}

// Next は、次の Event の列 (seq の昇順・最大 batch 件) を返す。尽きたら io.EOF。
func (b *Backfill) Next() ([]Event, error) {
	if b == nil || b.done {
		return nil, io.EOF
	}
	if b.h.w.trims.Load() != b.trims { // 読んでいる最中に GC が走った: 先頭の行が消えたかもしれないが、first_seq (hello) は古い。繋ぎ直せば、新しい first_seq で省略が分かる
		b.done = true
		return nil, ErrBackfillStale
	}
	rows, err := b.store.Range(b.cursor, b.before, b.batch)
	if err != nil {
		b.fail("読み取りの失敗: " + err.Error())
		return nil, ErrBackfillRead
	}
	if len(rows) == 0 {
		b.done = true
		if b.h.w.trims.Load() != b.trims {
			return nil, ErrBackfillStale
		}
		return nil, io.EOF
	}
	if len(rows) > b.batch {
		b.fail("Store が件数の上限を超えて返した")
		return nil, ErrBadStoredEvent
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		e, err := storedEvent(r, b.cursor, b.before)
		if err != nil {
			b.fail("不正な行 (seq " + fmt.Sprint(r.Seq) + "): " + err.Error())
			return nil, fmt.Errorf("%w: %v", ErrBadStoredEvent, err)
		}
		b.cursor = e.Seq
		out = append(out, e)
	}
	return out, nil
}

func (b *Backfill) fail(reason string) {
	b.done = true
	b.h.w.degrade(reason)
}

// storedEvent は、Store から読んだ行を検査して Event にする: seq は (after, before) の中で単調増加・payload は DefaultMaxEvent 以下・改行なし・JSON として妥当・
// JSON の seq・durable・type が行と合う (type は SSE の event: 行に使えない文字を含まない)。
func storedEvent(r StoredEvent, after, before uint64) (Event, error) {
	if r.Seq <= after || r.Seq >= before {
		return Event{}, errors.New("seq が範囲外 (または戻っている)")
	}
	if len(r.Payload) == 0 || len(r.Payload) > DefaultMaxEvent {
		return Event{}, errors.New("payload の大きさが範囲外")
	}
	if bytes.ContainsAny(r.Payload, "\r\n") {
		return Event{}, errors.New("payload に改行がある")
	}
	var m map[string]json.RawMessage // キー名を区別して読む (構造体への読みは大文字小文字を区別せず、UI の JSON.parse と食い違う)
	if err := json.Unmarshal(r.Payload, &m); err != nil {
		return Event{}, errors.New("JSON として読めない")
	}
	for k := range m {
		for _, want := range [...]string{"seq", "type", "durable"} {
			if k != want && strings.EqualFold(k, want) {
				return Event{}, errors.New("キー名の大文字小文字違いがある")
			}
		}
	}
	var f struct {
		Seq     *uint64
		Type    string
		Durable bool
	}
	if json.Unmarshal(m["seq"], &f.Seq) != nil || json.Unmarshal(m["type"], &f.Type) != nil || json.Unmarshal(m["durable"], &f.Durable) != nil {
		return Event{}, errors.New("seq・type・durable の型が不正")
	}
	if f.Seq == nil || *f.Seq != r.Seq || !f.Durable || !validEventType(f.Type) {
		return Event{}, errors.New("JSON の seq・durable・type が行と合わない")
	}
	return Event{Seq: r.Seq, Type: f.Type, Durable: true, JSON: r.Payload}, nil
}

func validEventType(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for _, c := range []byte(t) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

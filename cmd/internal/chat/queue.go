package chat

import (
	"errors"
	"io"
	"sync"
)

// QueuedWriter の上限の既定。
const (
	DefaultQueueLines = 256     // 積める行の数
	DefaultQueueBytes = 4 << 20 // 積める行の大きさの合計 (バイト)
)

var (
	// ErrQueueFull は、書き込みのキューが満杯 (エージェントが入力を読んでいない)。
	ErrQueueFull = errors.New("chat: エージェントの入力のキューが満杯 (エージェントが入力を読まない)")
	// ErrQueueClosed は、キューが閉じた (書き込みに失敗した後か、Close の後)。
	ErrQueueClosed = errors.New("chat: エージェントの入力のキューが閉じた")
)

// QueuedWriter は、エージェントの入力への書き込みを、有界のキューに積み、別の goroutine が書く。Conversation の Write は Mutex を持ったまま
// 呼ばれるので (ADR 0011)、エージェントが入力を読まなくても、呼び手を待たせない: 満杯なら ErrQueueFull を返す (Conversation は、
// それを書き込みの失敗として、会話を終える)。
//
// 会話が終わる条件: キューが満杯になるのは、エージェントが入力を読み続けず、積んだ行 (既定 256 行・4 MiB) を書き切れないとき。
// 要求の洪水 (未決の上限を超える要求の連続) は、要求ごとに自動拒否の 1 行を書くが、入力を読んでいるエージェントに対しては、
// 書き切れる速さで消化されるので、それだけでは満杯にならない (queue_test.go・conversation の洪水のテストで固定)。
type QueuedWriter struct {
	w        io.Writer
	maxLines int
	maxBytes int

	mu     sync.Mutex
	cond   *sync.Cond
	lines  [][]byte
	bytes  int
	closed bool
	err    error // 書き込みの失敗 (後の WriteLine が返す)
	done   chan struct{}
}

// NewQueuedWriter は、w に書く QueuedWriter を作り、書く goroutine を起こす。maxLines・maxBytes が 0 以下なら既定値。
// w の Write が失敗したら、キューは閉じ、onFail (nil でもよい) を、Mutex の外で 1 回呼ぶ。
func NewQueuedWriter(w io.Writer, maxLines, maxBytes int, onFail func(error)) *QueuedWriter {
	if maxLines <= 0 {
		maxLines = DefaultQueueLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultQueueBytes
	}
	q := &QueuedWriter{w: w, maxLines: maxLines, maxBytes: maxBytes, done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	go q.drain(onFail)
	return q
}

// WriteLine は、line (改行を含む 1 行) を、コピーして積む。待たない。満杯なら ErrQueueFull、閉じていれば ErrQueueClosed (書き込みの失敗があれば、その error)。
// 1 行だけで maxBytes を超える行は、積めない (ErrQueueFull)。
func (q *QueuedWriter) WriteLine(line []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		if q.err != nil {
			return q.err
		}
		return ErrQueueClosed
	}
	if len(q.lines) >= q.maxLines || q.bytes+len(line) > q.maxBytes {
		return ErrQueueFull
	}
	q.lines = append(q.lines, append([]byte(nil), line...))
	q.bytes += len(line)
	q.cond.Signal()
	return nil
}

// Close は、これ以上積まない。積んだ行は、書き切るまで (または書き込みが失敗するまで) 書く。何度呼んでもよい。
func (q *QueuedWriter) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Discard は、まだ書いていない行を捨てて閉じる (エージェントを止めるとき)。
func (q *QueuedWriter) Discard() {
	q.mu.Lock()
	q.closed = true
	q.lines, q.bytes = nil, 0
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Wait は、書く goroutine が終わる (閉じて、積んだ分を書き切った) まで待つ。
func (q *QueuedWriter) Wait() { <-q.done }

func (q *QueuedWriter) drain(onFail func(error)) {
	defer close(q.done)
	for {
		q.mu.Lock()
		for len(q.lines) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.lines) == 0 {
			q.mu.Unlock()
			return
		}
		line := q.lines[0]
		q.lines = q.lines[1:]
		q.bytes -= len(line)
		q.mu.Unlock()
		if _, err := q.w.Write(line); err != nil {
			q.mu.Lock()
			q.closed, q.err = true, err
			q.lines, q.bytes = nil, 0
			q.mu.Unlock()
			if onFail != nil {
				onFail(err)
			}
			return
		}
	}
}

//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
	"github.com/nananek/goronation/cmd/internal/eventlog"
)

// 耐久イベントログ (ADR 0023・0054) の、serve への配線: eventlog (同期の Store) を chat.EventStore にし、起動・縮退の通知・終了の削除を持つ。

// chatLogTotalLimit は、全セッションの DB ファイル (-wal を含む) の大きさの合計の上限: 掃除のあとに、これ以上なら、この起動は書かない (ADR 0027 決定 7)。
const chatLogTotalLimit = 256 << 20

// chatLogCloseWait は、終了のとき、書き手がキューを書き終える (または縮退する) のを待つ上限。
const chatLogCloseWait = 5 * time.Second

// chatEventStore は、*eventlog.Store を chat.EventStore にする (型の変換だけ。ロジックは足さない: ADR 0053 決定 3)。
type chatEventStore struct{ s *eventlog.Store }

func (c chatEventStore) Apply(op chat.StoreOp) error {
	rows := make([]eventlog.Row, len(op.Append))
	for i, e := range op.Append {
		rows[i] = eventlog.Row{Seq: e.Seq, Payload: e.Payload}
	}
	return c.s.Apply(eventlog.Op{Append: rows, Pin: op.Pin, Unpin: op.Unpin})
}

func (c chatEventStore) Range(after, before uint64, limit int) ([]chat.StoredEvent, error) {
	rows, err := c.s.Range(after, before, limit)
	if err != nil {
		return nil, err
	}
	out := make([]chat.StoredEvent, len(rows))
	for i, r := range rows {
		out[i] = chat.StoredEvent{Seq: r.Seq, Payload: r.Payload}
	}
	return out, nil
}

func (c chatEventStore) Trim(budgetBytes int64) (uint64, error) { return c.s.Trim(budgetBytes) }

// chatLog は、1 つの serve の耐久ログ。nil (--no-event-log・縮退して始めた) でも、メソッドは使える。
type chatLog struct {
	store  *eventlog.Store
	stderr io.Writer
}

// lockedWriter は、w への書き込みを直列にする (OnDegrade は別の goroutine から呼ばれる)。
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// oneLine は、運用者の端末に出す 1 行の文を作る: 制御文字を ? にし (sanitizeStderr)、改行も空白にする (Store の error は path を含みうる。画面・SSE には出さない)。
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(sanitizeStderr([]byte(s)))
}

// errChatLogLocked は、同じセッションの耐久ログを、別の serve が持っている (起動の失敗)。
var errChatLogLocked = errors.New("同じセッションの耐久ログを、別の goronation serve が持っている")

// chatSessionsDir は、全セッションの置き場所 (掃除の走査範囲)。
func chatSessionsDir(stateDir string) string {
	return filepath.Dir(filepath.Dir(termSocketPath(stateDir, defaultGroup, "x")))
}

// openChatLog は、耐久ログを開く。serve の起動ごとに 1 回、セッションの ID が決まった直後・檻を起こす前に呼ぶ (ADR 0054)。
// noLog (--no-event-log) なら nil。掃除のあと、全体の大きさが上限以上なら、この起動は書かない (nil)。Open の失敗は、ErrLocked だけが error
// (起動の失敗)・ほかは、標準エラー出力に 1 回出して nil (縮退して続ける)。DB の置き場所は、--socket と無関係に、セッションのディレクトリ。
func openChatLog(stateDir, id string, noLog bool, stderr io.Writer) (*chatLog, error) {
	if noLog {
		return nil, nil
	}
	out := &lockedWriter{w: stderr}
	degraded := func(format string, a ...any) (*chatLog, error) {
		fmt.Fprintf(out, "goronation serve: 耐久ログを使わない (履歴は、メモリのリングだけ): %s\n", oneLine(fmt.Sprintf(format, a...)))
		return nil, nil
	}
	_, total, err := eventlog.SweepStale(chatSessionsDir(stateDir))
	if err != nil {
		fmt.Fprintf(out, "goronation serve: 異常終了の残りを掃除できなかった: %s\n", oneLine(err.Error()))
	}
	if total >= chatLogTotalLimit {
		return degraded("全セッションの耐久ログの大きさが、上限 (%d MiB) を超えている", chatLogTotalLimit>>20)
	}
	dir := filepath.Dir(termSocketPath(stateDir, defaultGroup, id)) // --socket は見ない (掃除の走査範囲の外に、DB を作らない)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return degraded("セッションのディレクトリを作れない: %v", err)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return degraded("ログの ID を作れない: %v", err)
	}
	// Store の世代は、承認の世代 (Conversation.Generation) とは別の、DB の中だけの区別 (ADR 0054)。
	st, err := eventlog.Open(dir, hex.EncodeToString(b[:]), eventlog.Options{})
	if err != nil {
		if errors.Is(err, eventlog.ErrLocked) {
			return nil, errChatLogLocked
		}
		return degraded("開けない: %v", err)
	}
	return &chatLog{store: st, stderr: out}, nil
}

// config は、chat.SessionConfig の Store・OnDegrade (nil の chatLog は、どちらも nil)。OnDegrade の reason は、Store の error (path を含みうる) を含むので、
// 運用者の標準エラー出力だけに、無害化して出す (画面・SSE には出さない)。
func (l *chatLog) config() (chat.EventStore, func(string)) {
	if l == nil {
		return nil, nil
	}
	return chatEventStore{l.store}, func(reason string) {
		fmt.Fprintf(l.stderr, "goronation serve: 耐久ログを止めた (以後の履歴は、メモリのリングだけ): %s\n", oneLine(reason))
	}
}

// Close は、書き手がキューを書き終える (または縮退する) のを、chatLogCloseWait まで待ってから、DB・-wal・-shm を消してロックを手放す。
// hub は、すでに Close されている前提 (されていなければ、待ちは時間切れになり、そのまま閉じる)。
func (l *chatLog) Close(hub *chat.Hub) error {
	if l == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), chatLogCloseWait)
	defer cancel()
	hub.WaitStore(ctx)
	return l.store.Close(true)
}

// abort は、起動に失敗したときに、DB を残さずに閉じる (書き手を待たない。会話が始まっていなければ、書くものが無い)。
func (l *chatLog) abort() {
	if l != nil {
		l.store.Close(true)
	}
}

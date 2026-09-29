package chat

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

// DefaultMaxLine は、LineReader の 1 行の上限の既定 (バイト。改行を除く)。未決の権限要求は input を最大で 1 行分保持するので、
// 未決の数の上限 (agent/claude の maxPendingRequests=64) との積 (約 64 MiB) が、要求の保持の最悪のメモリ量になる (ADR 0009)。
const DefaultMaxLine = 1 << 20

var (
	// ErrLineTooLong は、行が上限を超えた (その行は改行まで読み捨てた。次の Next は次の行から読む)。
	ErrLineTooLong = errors.New("chat: 行が上限を超えた (捨てた)")
	// ErrBadLine は、行に NUL か不正な UTF-8 がある (その行は捨てた。次の Next は次の行から読む)。
	ErrBadLine = errors.New("chat: 行に NUL か不正な UTF-8 がある (捨てた)")
)

// LineReader は、エージェントの標準出力 (敵対入力) を、1 行ずつ読む。行の長さに上限があり、上限を超える行は、貯めずに
// 改行まで読み捨てる (改行の無い無限の出力でも、メモリは上限までしか使わない)。並行には呼べない。
type LineReader struct {
	br  *bufio.Reader
	max int
	buf []byte
}

// NewLineReader は、r を読む LineReader を作る。max は 1 行の上限 (改行を除くバイト。0 以下なら DefaultMaxLine)。
func NewLineReader(r io.Reader, max int) *LineReader {
	if max <= 0 {
		max = DefaultMaxLine
	}
	return &LineReader{br: bufio.NewReaderSize(r, 4096), max: max}
}

// Next は、次の行 (改行と、その前の CR 1 つを除く) を返す。返すスライスは、次の Next まで有効。空行は飛ばす (続けて上限個を超える空行は、ErrBadLine で一度戻る)。
// 上限を超えた行は ErrLineTooLong、NUL か不正な UTF-8 を含む行は ErrBadLine (どちらも、その行を捨てただけで、続けて読める)。
// 改行で終わらない最後の行も、1 行として返す。読み切ったら io.EOF。読み込みの error は、そのまま返す。
func (l *LineReader) Next() ([]byte, error) {
	blank := 0
	for {
		l.buf = l.buf[:0]
		over := false
		var readErr error
		for {
			chunk, err := l.br.ReadSlice('\n')
			if !over {
				if len(l.buf)+len(chunk) > l.max+2 { // +2: 改行と CR の分
					over, l.buf = true, l.buf[:0]
				} else {
					l.buf = append(l.buf, chunk...)
				}
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			readErr = err
			break
		}
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if readErr == io.EOF && !over && len(l.buf) == 0 {
			return nil, io.EOF
		}
		if over {
			return nil, ErrLineTooLong
		}
		line := bytes.TrimSuffix(l.buf, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) > l.max {
			return nil, ErrLineTooLong
		}
		if len(line) == 0 {
			// 空行だけの洪水で、呼び手に戻らないまま回り続けない。続けて max 個の空行を読んだら、error で一度戻す。
			if blank++; blank > l.max {
				return nil, ErrBadLine
			}
			continue
		}
		if bytes.IndexByte(line, 0) >= 0 || !utf8.Valid(line) {
			return nil, ErrBadLine
		}
		return line, nil
	}
}

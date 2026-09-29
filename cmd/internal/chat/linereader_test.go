package chat

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, lr *LineReader) (lines []string, errs []error) {
	t.Helper()
	for {
		l, err := lr.Next()
		switch {
		case err == io.EOF:
			return
		case err != nil:
			errs = append(errs, err)
		default:
			lines = append(lines, string(l))
		}
	}
}

func TestLineReaderBasic(t *testing.T) {
	in := "a\r\n\nb\n\r\nc" // CRLF・空行・改行で終わらない最後の行
	lines, errs := readAll(t, NewLineReader(strings.NewReader(in), 0))
	if len(errs) != 0 || strings.Join(lines, "|") != "a|b|c" {
		t.Fatalf("lines=%q errs=%v", lines, errs)
	}
}

func TestLineReaderLimit(t *testing.T) {
	const max = 10
	in := strings.Repeat("x", max) + "\n" + strings.Repeat("y", max+1) + "\nok\n" + strings.Repeat("z", 3*4096) + "\nend"
	lines, errs := readAll(t, NewLineReader(strings.NewReader(in), max))
	if strings.Join(lines, "|") != strings.Repeat("x", max)+"|ok|end" {
		t.Fatalf("lines=%q", lines)
	}
	if len(errs) != 2 || !errors.Is(errs[0], ErrLineTooLong) || !errors.Is(errs[1], ErrLineTooLong) {
		t.Fatalf("errs=%v", errs)
	}
}

// endless は、改行の無い 'a' を n バイト返し、そのあと tail を返す。
type endless struct {
	n    int
	tail string
}

func (e *endless) Read(p []byte) (int, error) {
	if e.n > 0 {
		k := min(len(p), e.n)
		for i := 0; i < k; i++ {
			p[i] = 'a'
		}
		e.n -= k
		return k, nil
	}
	if e.tail == "" {
		return 0, io.EOF
	}
	k := copy(p, e.tail)
	e.tail = e.tail[k:]
	return k, nil
}

// 改行の無い巨大な出力を、貯めずに捨てる (メモリは上限までしか使わない)。そのあとの行は読める。
func TestLineReaderDropsHugeLineWithoutBuffering(t *testing.T) {
	const max = 1024
	lr := NewLineReader(&endless{n: 256 << 20, tail: "\nnext\n"}, max)
	if _, err := lr.Next(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err=%v", err)
	}
	if cap(lr.buf) > max+8192 {
		t.Fatalf("バッファが上限を超えて育った: cap=%d", cap(lr.buf))
	}
	l, err := lr.Next()
	if err != nil || string(l) != "next" {
		t.Fatalf("line=%q err=%v", l, err)
	}
}

func TestLineReaderBadLines(t *testing.T) {
	in := "a\x00b\n\xff\xfe\nfine\n\"\xc3\x28\"\n"
	lines, errs := readAll(t, NewLineReader(strings.NewReader(in), 0))
	if strings.Join(lines, "|") != "fine" || len(errs) != 3 {
		t.Fatalf("lines=%q errs=%v", lines, errs)
	}
	for _, e := range errs {
		if !errors.Is(e, ErrBadLine) {
			t.Fatalf("err=%v", e)
		}
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestLineReaderReadError(t *testing.T) {
	want := errors.New("boom")
	r := io.MultiReader(strings.NewReader("partial"), errReader{want})
	if _, err := NewLineReader(r, 0).Next(); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
}

func FuzzLineReader(f *testing.F) {
	f.Add([]byte("a\nb\r\n\x00\n\xff\n"), uint8(4))
	f.Add(bytes.Repeat([]byte("x"), 9000), uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, m uint8) {
		max := int(m) + 1
		lr := NewLineReader(bytes.NewReader(data), max)
		total := 0
		for i := 0; i <= len(data)+1; i++ {
			l, err := lr.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				continue
			}
			if len(l) == 0 || len(l) > max || bytes.IndexByte(l, 0) >= 0 || bytes.IndexByte(l, '\n') >= 0 {
				t.Fatalf("不正な行: %q (max=%d)", l, max)
			}
			total += len(l)
		}
		t.Fatalf("読み切れない (data=%d バイト)", len(data))
	})
}

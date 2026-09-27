package git

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Limits は、receive-pack の要求の、コマンド部 (pack より前) の上限。0 以下の項目は、既定値になる。
type Limits struct {
	// MaxBytes は、コマンド部の全体 (pkt-line の長さの 4 桁と、終わりの 0000 を含む) の上限 (バイト)。
	MaxBytes int
	// MaxCommands は、コマンド (更新する ref) の件数の上限。
	MaxCommands int
}

// 既定の上限。実際の push は、1 件で 200 バイトほど。
const (
	DefaultMaxBytes    = 16 << 10
	DefaultMaxCommands = 32
)

// pkt-line 1 件の、長さの 4 桁を含む上限 (git の LARGE_PACKET_MAX)。
const maxPkt = 65520

// zeroOID は、ref が無いことを表す oid (SHA-1)。create の old、delete の new。
const zeroOID = "0000000000000000000000000000000000000000"

// Command は、receive-pack の 1 つのコマンド (ref の更新 1 件)。oid は、40 桁の小文字の 16 進。
type Command struct {
	Old, New, Ref string
}

// Push は、検査を通した receive-pack の要求のコマンド部。
type Push struct {
	// Commands は、更新する ref。空なら、探り (git が大きい push の前に送る、0000 だけの本文) で、何も更新しない。
	Commands []Command
	// Caps は、クライアントが宣言した capability (許可した名前だけ)。
	Caps []string
	// Raw は、読んだバイト列そのもの (コマンド部の全体)。上流には、Raw の後に、残りの本文 (pack) をそのまま続ける。
	// 解釈し直した値を送らない: 検査した内容と、上流が解釈する内容の食い違いを、作らないため。
	Raw []byte
}

// Probe は、p が探り (コマンドが無い) か。
func (p *Push) Probe() bool { return len(p.Commands) == 0 }

// ParseCommands は、receive-pack の要求の本文 r から、コマンド部 (最後の 0000 まで) だけを読んで、検査する。
// 読むのは、コマンド部の終わりまで、かつ Limits の範囲だけで、pack は読まない (呼び手が、Raw の後に、r の残りを続ける)。
// 形式が正しくなければ、*Error を返す (Code は Code* の定数)。読み取り自体の失敗は、そのまま返す (時間の上限は、呼び手が r の側に付ける)。
// 文法は、実際の git が送る形に合わせて厳格にした。1 通りの書き方だけを通し (canonical)、Raw を作り直すと、入力と同じ値になる。
//   - pkt-line の長さは、小文字の 16 進 4 桁で、5 以上 65520 以下。0000 (flush) は、コマンド部の終わりだけ。
//   - 行は "old SP new SP ref"。oid は 40 桁の小文字の 16 進。ref は、印字できる ASCII (空白を含まない) の 255 バイトまで。行末の LF は無い。
//   - NUL は、先頭の行の ref の後に 1 回だけ (capability が無ければ付けない)。続くのは " cap cap ..." (先頭に空白 1 つ・区切りは空白 1 つ)。
//   - capability は、report-status・report-status-v2・side-band-64k・quiet・atomic・ofs-delta・object-format=sha1・agent=<値> だけ。重複は断る。
//   - shallow・push-cert・push-options は断る。コマンドが 0 件 (探り) なら、本文はそこで終わっていなければならない。
func ParseCommands(r io.Reader, lim Limits) (*Push, error) {
	if lim.MaxBytes <= 0 {
		lim.MaxBytes = DefaultMaxBytes
	}
	if lim.MaxCommands <= 0 {
		lim.MaxCommands = DefaultMaxCommands
	}
	p := &pktReader{r: r, max: lim.MaxBytes}
	push := &Push{}
	for {
		line, flush, err := p.next()
		if err != nil {
			return nil, err
		}
		if flush {
			break
		}
		if len(push.Commands) >= lim.MaxCommands {
			return nil, reject(CodeTooManyCmds, "コマンドが %d 件を超える", lim.MaxCommands)
		}
		cmd, caps, err := parseLine(line, len(push.Commands) == 0)
		if err != nil {
			return nil, err
		}
		if len(push.Commands) == 0 {
			push.Caps = caps
		}
		push.Commands = append(push.Commands, cmd)
	}
	if len(push.Commands) == 0 {
		// 探り。何かが続くなら、上流が pack として読む前に断る。
		var b [1]byte
		switch _, err := io.ReadFull(r, b[:]); {
		case err == nil:
			return nil, reject(CodeTrailingData, "コマンドが無いのに、本文が続く")
		case !errors.Is(err, io.EOF):
			return nil, fmt.Errorf("git: 本文を読めない: %w", err)
		}
	}
	push.Raw = p.raw
	return push, nil
}

// pktReader は、pkt-line を 1 件ずつ読み、読んだバイト列を raw に貯める。
type pktReader struct {
	r   io.Reader
	max int
	raw []byte
}

// readN は、ちょうど n バイトを読む (max を超えるなら、読まずに断る)。本文が途中で終われば、CodeTruncated。
func (p *pktReader) readN(n int) ([]byte, error) {
	if len(p.raw)+n > p.max {
		return nil, reject(CodeTooLarge, "コマンド部が %d バイトを超える", p.max)
	}
	off := len(p.raw)
	p.raw = append(p.raw, make([]byte, n)...)
	if _, err := io.ReadFull(p.r, p.raw[off:]); err != nil {
		p.raw = p.raw[:off]
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, reject(CodeTruncated, "本文がコマンド部の途中で終わった")
		}
		return nil, fmt.Errorf("git: 本文を読めない: %w", err)
	}
	return p.raw[off:], nil
}

// next は、pkt-line を 1 件読む。0000 (flush) なら flush = true。行の内容は、次の next まで有効。
func (p *pktReader) next() (line []byte, flush bool, err error) {
	h, err := p.readN(4)
	if err != nil {
		return nil, false, err
	}
	n := 0
	for _, c := range h {
		switch {
		case c >= '0' && c <= '9':
			n = n<<4 | int(c-'0')
		case c >= 'a' && c <= 'f':
			n = n<<4 | int(c-'a'+10)
		default: // 大文字の 16 進も、git は読むので、食い違いの元になる
			return nil, false, reject(CodeBadPacket, "長さ %s が、小文字の 16 進 4 桁ではない", q(string(h)))
		}
	}
	switch {
	case n == 0:
		return nil, true, nil
	case n <= 4: // 0001 (delim)・0002 (response-end)・0003・0004 (空の行)。コマンド部には、どれも要らない
		return nil, false, reject(CodeBadPacket, "長さ %s は、コマンド部に使えない", q(string(h)))
	case n > maxPkt:
		return nil, false, reject(CodeBadPacket, "長さ %s が %d を超える", q(string(h)), maxPkt)
	}
	body, err := p.readN(n - 4)
	if err != nil {
		return nil, false, err
	}
	return body, false, nil
}

// parseLine は、コマンドの 1 行を検査する。first は、先頭の行か (capability は、先頭の行にだけ付く)。
func parseLine(line []byte, first bool) (Command, []string, error) {
	s := string(line)
	switch {
	case strings.HasPrefix(s, "shallow "):
		return Command{}, nil, reject(CodeShallow, "shallow の行は受けない")
	case strings.HasPrefix(s, "push-cert"):
		return Command{}, nil, reject(CodePushCert, "push-cert は受けない")
	}
	cmdPart, capPart, hasNUL := strings.Cut(s, "\x00")
	if hasNUL && !first {
		return Command{}, nil, reject(CodeBadCommand, "NUL は先頭の行だけ")
	}
	// "old SP new SP ref": 40 + 1 + 40 + 1 + (1 以上)
	if len(cmdPart) < 83 || cmdPart[40] != ' ' || cmdPart[81] != ' ' {
		return Command{}, nil, reject(CodeBadCommand, "行 %s が old new ref の形ではない", q(cmdPart))
	}
	cmd := Command{Old: cmdPart[:40], New: cmdPart[41:81], Ref: cmdPart[82:]}
	if !validOID(cmd.Old) || !validOID(cmd.New) {
		return Command{}, nil, reject(CodeBadCommand, "oid が、40 桁の小文字の 16 進ではない: %s", q(cmdPart[:81]))
	}
	if len(cmd.Ref) > 255 || !printable(cmd.Ref) {
		return Command{}, nil, reject(CodeBadCommand, "ref %s が、印字できる ASCII の 255 バイト以内ではない", q(cmd.Ref))
	}
	if !hasNUL {
		return cmd, nil, nil
	}
	caps, err := parseCaps(capPart)
	return cmd, caps, err
}

// parseCaps は、NUL の後の文字列 (" cap cap ...") を検査し、capability の一覧を返す。
func parseCaps(s string) ([]string, error) {
	if s == "" || s[0] != ' ' { // git は、capability が無いときは NUL も付けない (NUL だけの行は、書き方が 2 通りになる)
		return nil, reject(CodeBadCapability, "capability の一覧 %s が、空白で始まる形ではない", q(s))
	}
	var caps []string
	seen := map[string]bool{}
	for _, c := range strings.Split(s[1:], " ") {
		name, _, _ := strings.Cut(c, "=")
		switch {
		case c == "push-options":
			return nil, reject(CodePushOptions, "push-options は受けない")
		case !capAllowed(c):
			return nil, reject(CodeBadCapability, "capability %s は許可していない", q(c))
		case seen[name]:
			return nil, reject(CodeBadCapability, "capability %s が重複している", q(name))
		}
		seen[name] = true
		caps = append(caps, c)
	}
	return caps, nil
}

// capAllowed は、capability c (agent=<値> は、値まで) を許可しているか。
func capAllowed(c string) bool {
	switch c {
	case "report-status", "report-status-v2", "side-band-64k", "quiet", "atomic", "ofs-delta", "object-format=sha1":
		return true
	}
	// git は、agent の値の空白などを "." に置き換えて送る。
	if v, ok := strings.CutPrefix(c, "agent="); ok {
		return v != "" && len(v) <= 128 && printable(v)
	}
	return false
}

// validOID は、40 桁の小文字の 16 進か。
func validOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// printable は、空白を含まない、印字できる ASCII (0x21〜0x7e) だけか。
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

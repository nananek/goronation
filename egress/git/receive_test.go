package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"unicode"
	"unicode/utf8"
)

const (
	sess = "20260927-041500-a1b2c3"
	pfx  = "refs/heads/goronation/" + sess + "/"

	capsFull  = " report-status-v2 side-band-64k quiet object-format=sha1 agent=git/2.47.3"
	oid1      = "ed917376351e2f55b638e7c8cf43d36819b9105c"
	oid2      = "985fd8e7b6dcf5e69904b8184d11c198ace45a13"
	oid3      = "537be356c20fb52941e94d86e7cdcf804cdff8f5"
	oid4      = "b7b43428788a5ac35056b4238bb20052cab2bf71"
	oid5      = "86816457a1c509837e2d0158e95c171932ae2d40"
	capturedD = "testdata/capture/git-2.47.3"
)

// pkt は、s を pkt-line 1 件にする。
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// cmdLine は、"old new ref" の行。
func cmdLine(old, new, ref string) string { return old + " " + new + " " + ref }

// body は、行 (pkt-line にする) を並べ、flush で閉じる。
func body(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(pkt(l))
	}
	return b.String() + "0000"
}

// encode は、Push を canonical な書き方 (git が送る形) に戻す。ParseCommands が通したものは、Raw と一致しなければならない。
func encode(p *Push) string {
	var lines []string
	for i, c := range p.Commands {
		s := cmdLine(c.Old, c.New, c.Ref)
		if i == 0 && len(p.Caps) > 0 {
			s += "\x00 " + strings.Join(p.Caps, " ")
		}
		lines = append(lines, s)
	}
	return body(lines...)
}

// safeText は、端末に出してよい文字列か: 有効な UTF-8 で、制御文字・書式制御文字・行区切り・(ASCII の空白を除く) 空白の類を含まず、
// 非 ASCII は、固定の文言に使う文字 (かな・漢字・全角の記号) だけ。要求由来の文字は、q が ASCII に直す。
func safeText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == ' ' || r > 0x20 && r < 0x7f:
		case r < 0x20 || r >= 0x7f && r < 0x100:
			return false
		case unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han), r == 0x30fb, r == 0x30fc, r >= 0x3001 && r <= 0x303f, r >= 0xff01 && r <= 0xff5e:
		default:
			return false
		}
	}
	return true
}

var knownCodes = map[string]bool{
	CodeBadRoute: true, CodeBadPacket: true, CodeTruncated: true, CodeTooLarge: true, CodeTooManyCmds: true,
	CodeBadCommand: true, CodeBadCapability: true, CodePushOptions: true, CodeShallow: true, CodePushCert: true,
	CodeTrailingData: true, CodeDelete: true, CodeRefNotAllowed: true, CodeRepoNotAllowed: true,
}

// checkInvariants は、どんな入力でも成り立つ性質を確かめる (fuzz も使う)。
func checkInvariants(t testing.TB, data []byte, push *Push, err error) {
	t.Helper()
	if err != nil {
		if push != nil {
			t.Fatalf("エラーなのに Push を返した")
		}
		var e *Error
		if errors.As(err, &e) {
			if !knownCodes[e.Code] {
				t.Fatalf("未知の Code %q", e.Code)
			}
			if !safeText(err.Error()) {
				t.Fatalf("エラーの文字列に、制御文字か非 ASCII がある: %q", err.Error())
			}
			if len(err.Error()) > 400 {
				t.Fatalf("エラーの文字列が長い (%d)", len(err.Error()))
			}
		}
		return
	}
	if !bytes.HasPrefix(data, push.Raw) {
		t.Fatalf("Raw が入力の先頭ではない")
	}
	if len(push.Raw) > DefaultMaxBytes || len(push.Commands) > DefaultMaxCommands {
		t.Fatalf("上限を超えたものを通した: %d バイト・%d 件", len(push.Raw), len(push.Commands))
	}
	if got := encode(push); got != string(push.Raw) {
		t.Fatalf("通したのに canonical ではない:\n Raw    %q\n encode %q", push.Raw, got)
	}
	if push.Probe() && len(data) != 4 {
		t.Fatalf("探りなのに、本文が 0000 だけではない: %q", data)
	}
	for _, c := range push.Commands {
		if !validOID(c.Old) || !validOID(c.New) || !printable(c.Ref) || c.Ref == "" {
			t.Fatalf("不正なコマンドを通した: %+v", c)
		}
	}
	for _, c := range push.Caps {
		if !capAllowed(c) {
			t.Fatalf("不正な capability を通した: %q", c)
		}
	}
}

func cmds(cs ...Command) []Command { return cs }

func zero() string { return zeroOID }

// TestParseCommandsCaptured は、実物の git (2.47.3) が、偽の上流に送った本文 (testdata/capture の採取。手順は capture.sh) を通す。
// 通す・断るの結果と、通したコマンドを、独立に (Python で) 読んだ値と照合する。
func TestParseCommandsCaptured(t *testing.T) {
	type want struct {
		code string // 空なら、通る
		caps string
		cmds []Command
		pack bool // コマンド部の後に pack が続く (削除だけの push は続かない)
	}
	create := func(new, ref string) Command { return Command{zero(), new, ref} }
	full := capsFull
	cases := map[string]want{
		"create":   {caps: full, pack: true, cmds: cmds(create(oid1, pfx+"one"))},
		"update":   {caps: full, pack: true, cmds: cmds(Command{oid1, oid2, pfx + "one"})},
		"progress": {caps: " report-status-v2 side-band-64k object-format=sha1 agent=git/2.47.3", pack: true, cmds: cmds(Command{oid2, oid3, pfx + "one"})},
		"multi":    {caps: full, pack: true, cmds: cmds(create(oid3, pfx+"a"), create(oid3, pfx+"b"))},
		"atomic": {caps: " report-status-v2 side-band-64k quiet atomic object-format=sha1 agent=git/2.47.3", pack: true,
			cmds: cmds(Command{oid3, oid4, pfx + "a"}, Command{oid3, oid4, pfx + "b"})},
		"copy":     {caps: full, pack: true, cmds: cmds(create(oid4, pfx+"copy"))},
		"force":    {caps: full, pack: true, cmds: cmds(Command{oid4, oid5, pfx + "a"})}, // force は、書式では区別できない
		"delete":   {caps: full, pack: false, cmds: cmds(Command{oid4, zero(), pfx + "copy"})},
		"tag":      {caps: full, pack: true, cmds: cmds(create("8469dd9940e15fc1b88ce8edcc824d2d1a1c7a2b", "refs/tags/t1"))},
		"lighttag": {caps: full, pack: true, cmds: cmds(create(oid5, "refs/tags/t2"))},
		"main":     {caps: full, pack: true, cmds: cmds(create(oid5, "refs/heads/main"))},
		"nested":   {caps: full, pack: true, cmds: cmds(create(oid5, pfx+"feat/x/y"))},
		"mixed":    {caps: full, pack: true, cmds: cmds(create(oid5, pfx+"ok2"), create(oid5, "refs/heads/other"))},
		"big":      {caps: full, pack: true, cmds: cmds(create("5e2b0f9e71a300716da181ba0544cd63775b6b6f", pfx+"big"))},
		// 大きい push の前に、git が、0000 だけの本文を POST する (認証の探り)。
		"big-probe": {},
		"pushopt":   {code: CodePushOptions},
		"shallow":   {code: CodeShallow},
		"signed":    {code: CodePushCert},
		"unicode":   {code: CodeBadCommand},
		"sha256":    {code: CodeBadCommand},
	}
	files, err := filepath.Glob(filepath.Join(capturedD, "*.bin"))
	if err != nil || len(files) != len(cases) {
		t.Fatalf("採取したファイルは %d 個のはず: %v (%v)", len(cases), files, err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".bin")
		w, ok := cases[name]
		if !ok {
			t.Fatalf("%s の期待が無い", name)
		}
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for mode, wrap := range map[string]func(io.Reader) io.Reader{
				"plain":   func(r io.Reader) io.Reader { return r },
				"onebyte": iotest.OneByteReader,
				"dataerr": iotest.DataErrReader,
			} {
				r := bytes.NewReader(data)
				push, err := ParseCommands(wrap(r), Limits{})
				checkInvariants(t, data, push, err)
				if w.code != "" {
					if Reason(err) != w.code {
						t.Fatalf("%s: Code = %q, 期待 %q (err = %v)", mode, Reason(err), w.code, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: 通るはずが、断られた: %v", mode, err)
				}
				if name == "big-probe" {
					if !push.Probe() || string(push.Raw) != "0000" {
						t.Fatalf("探りが通らない: %+v", push)
					}
					continue
				}
				if !reflect.DeepEqual(push.Commands, w.cmds) {
					t.Fatalf("%s: コマンド\n got  %+v\n want %+v", mode, push.Commands, w.cmds)
				}
				if got := " " + strings.Join(push.Caps, " "); got != w.caps {
					t.Fatalf("%s: capability = %q, 期待 %q", mode, got, w.caps)
				}
				// pack の手前で止まる: 残りは、そのまま pack (削除だけなら、何も無い)。
				rest := data[len(push.Raw):]
				if w.pack != bytes.HasPrefix(rest, []byte("PACK")) || (!w.pack && len(rest) != 0) {
					t.Fatalf("%s: コマンド部の後 = %q", mode, rest[:min(len(rest), 8)])
				}
				if mode == "plain" {
					left, _ := io.ReadAll(r)
					if !bytes.Equal(left, rest) {
						t.Fatalf("読み過ぎ: r の残りが pack の手前から始まらない (%d バイト残り、期待 %d)", len(left), len(rest))
					}
				}
			}
		})
	}
}

// TestParseCommandsReject は、実物には現れない、敵対的な書き方を断る。1 件ずつ、理由 (Code) まで固定する。
func TestParseCommandsReject(t *testing.T) {
	ok := cmdLine(zero(), oid1, pfx+"x")
	first := ok + "\x00" + capsFull
	upper := func(s string) string { // 長さの 16 進を、大文字にする
		u := strings.ToUpper(s[:4]) + s[4:]
		if u == s {
			t.Fatalf("大文字にできない長さ: %q", s[:4])
		}
		return u
	}
	long := func(n int) string { return pfx + strings.Repeat("a", n-len(pfx)) }
	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty", "", CodeTruncated},
		{"short-header", "00", CodeTruncated},
		{"no-flush", pkt(first), CodeTruncated},
		{"cut-payload", pkt(first)[:50], CodeTruncated},
		{"only-header", "0087", CodeTruncated},

		{"upper-hex-length", upper(pkt(first)) + "0000", CodeBadPacket},
		{"non-hex-length", "zzzz" + first + "0000", CodeBadPacket},
		{"sign-length", "+0ab" + first + "0000", CodeBadPacket},
		{"space-length", " 0ab" + first + "0000", CodeBadPacket},
		{"delim-0001", "0001" + "0000", CodeBadPacket},
		{"response-end-0002", "0002" + "0000", CodeBadPacket},
		{"length-0003", "0003" + "0000", CodeBadPacket},
		{"empty-line-0004", "0004" + "0000", CodeBadPacket},
		{"length-65521", "fff1" + "0000", CodeBadPacket},
		{"length-ffff", "ffff" + "0000", CodeBadPacket},
		{"length-65520", "fff0", CodeTooLarge},

		{"oid-39", body(cmdLine(zero()[:39], oid1, pfx+"x")), CodeBadCommand},
		{"oid-uppercase", body(cmdLine(zero(), strings.ToUpper(oid1), pfx+"x")), CodeBadCommand},
		{"oid-non-hex", body(cmdLine(zero(), strings.Repeat("g", 40), pfx+"x")), CodeBadCommand},
		{"oid-sha256", body(cmdLine(strings.Repeat("0", 64), strings.Repeat("1", 64), pfx+"x")), CodeBadCommand},
		{"no-ref", body(zero() + " " + oid1 + " "), CodeBadCommand},
		{"no-ref-no-space", body(zero() + " " + oid1), CodeBadCommand},
		{"ref-with-space", body(ok + " y"), CodeBadCommand},
		{"ref-with-lf", body(ok + "\n"), CodeBadCommand},
		{"ref-with-esc", body(ok + "\x1b[31m"), CodeBadCommand},
		{"ref-non-ascii", body(ok + "\xe6\x97\xa5"), CodeBadCommand},
		{"ref-with-del", body(ok + "\x7f"), CodeBadCommand},
		{"ref-256", body(cmdLine(zero(), oid1, long(256))), CodeBadCommand},
		{"double-space", body(zero() + "  " + oid1 + " " + pfx + "x"), CodeBadCommand},
		{"tab-separator", body(zero() + "\t" + oid1 + " " + pfx + "x"), CodeBadCommand},
		{"tab-second-separator", body(zero() + " " + oid1 + "\t" + pfx + "x"), CodeBadCommand},
		{"old-uppercase", body(cmdLine(strings.ToUpper(oid1), oid2, pfx+"x")), CodeBadCommand},
		{"nul-in-second-line", body(first, ok+"\x00"+capsFull), CodeBadCommand},
		{"nul-in-second-line-empty", body(first, ok+"\x00"), CodeBadCommand},

		{"caps-no-leading-space", body(ok + "\x00report-status"), CodeBadCapability},
		{"caps-junk-prefix", body(ok + "\x00Xquiet"), CodeBadCapability},
		{"caps-empty-after-nul", body(ok + "\x00"), CodeBadCapability},
		{"caps-space-only", body(ok + "\x00 "), CodeBadCapability},
		{"caps-double-space", body(ok + "\x00 report-status  quiet"), CodeBadCapability},
		{"caps-trailing-space", body(ok + "\x00 report-status "), CodeBadCapability},
		{"caps-unknown", body(ok + "\x00 report-status frobnicate"), CodeBadCapability},
		{"caps-duplicate", body(ok + "\x00 quiet quiet"), CodeBadCapability},
		{"caps-duplicate-agent", body(ok + "\x00 agent=a agent=b"), CodeBadCapability},
		{"caps-agent-empty", body(ok + "\x00 agent="), CodeBadCapability},
		{"caps-agent-esc", body(ok + "\x00 agent=x\x1b[0m"), CodeBadCapability},
		{"caps-agent-129", body(ok + "\x00 agent=" + strings.Repeat("a", 129)), CodeBadCapability},
		{"caps-second-nul", body(ok + "\x00 quiet\x00 atomic"), CodeBadCapability},
		{"caps-sha256", body(ok + "\x00 object-format=sha256"), CodeBadCapability},
		{"caps-case", body(ok + "\x00 Report-Status"), CodeBadCapability},
		{"caps-delete-refs", body(ok + "\x00 delete-refs"), CodeBadCapability},
		{"caps-push-options", body(ok + "\x00 report-status push-options"), CodePushOptions},

		{"shallow", body("shallow "+oid1+"\n", first), CodeShallow},
		{"shallow-later", body(first, "shallow "+oid1+"\n"), CodeShallow},
		{"push-cert", body("push-cert\x00" + capsFull), CodePushCert},
		{"push-cert-plain", body("push-cert", "certificate version 0.1\n", "push-cert-end\n"), CodePushCert},

		{"probe-then-data", "0000x", CodeTrailingData},
		{"probe-then-pack", "0000PACK", CodeTrailingData},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			push, err := ParseCommands(strings.NewReader(c.body), Limits{})
			checkInvariants(t, []byte(c.body), push, err)
			if err == nil {
				t.Fatalf("断るはずが通った: %+v", push)
			}
			if Reason(err) != c.code {
				t.Fatalf("Code = %q, 期待 %q (%v)", Reason(err), c.code, err)
			}
		})
	}
}

// TestParseCommandsAccepts は、境界の、通る書き方を固定する。
func TestParseCommandsAccepts(t *testing.T) {
	ok := cmdLine(zero(), oid1, pfx+"x")
	cases := []struct {
		name string
		body string
		n    int
	}{
		{"no-caps", body(ok), 1},
		{"agent-with-dots", body(ok + "\x00 agent=git/2.39.5.(Apple.Git-154)"), 1},
		{"agent-128", body(ok + "\x00 agent=" + strings.Repeat("a", 128)), 1},
		{"all-caps", body(ok + "\x00 report-status report-status-v2 side-band-64k quiet atomic ofs-delta object-format=sha1 agent=x"), 1},
		{"ref-255", body(cmdLine(zero(), oid1, pfx+strings.Repeat("a", 255-len(pfx)))), 1},
		{"32-commands", body(repeatCmds(32)...), 32},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			push, err := ParseCommands(strings.NewReader(c.body), Limits{})
			checkInvariants(t, []byte(c.body), push, err)
			if err != nil || len(push.Commands) != c.n {
				t.Fatalf("通るはず: %v %+v", err, push)
			}
		})
	}
}

func repeatCmds(n int) []string {
	var lines []string
	for i := range n {
		l := cmdLine(zero(), oid1, fmt.Sprintf("%sr%d", pfx, i))
		if i == 0 {
			l += "\x00" + capsFull
		}
		lines = append(lines, l)
	}
	return lines
}

// TestParseCommandsLimits は、件数と大きさの上限が、境界どおりに働くことを確かめる。
func TestParseCommandsLimits(t *testing.T) {
	t.Run("33-commands", func(t *testing.T) {
		_, err := ParseCommands(strings.NewReader(body(repeatCmds(33)...)), Limits{})
		if Reason(err) != CodeTooManyCmds {
			t.Fatalf("Code = %q (%v)", Reason(err), err)
		}
	})
	t.Run("custom-commands", func(t *testing.T) {
		if _, err := ParseCommands(strings.NewReader(body(repeatCmds(2)...)), Limits{MaxCommands: 2}); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseCommands(strings.NewReader(body(repeatCmds(3)...)), Limits{MaxCommands: 2}); Reason(err) != CodeTooManyCmds {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bytes-boundary", func(t *testing.T) {
		b := body(repeatCmds(1)...)
		if _, err := ParseCommands(strings.NewReader(b), Limits{MaxBytes: len(b)}); err != nil {
			t.Fatalf("ちょうどの大きさは通る: %v", err)
		}
		_, err := ParseCommands(strings.NewReader(b), Limits{MaxBytes: len(b) - 1})
		if Reason(err) != CodeTooLarge {
			t.Fatalf("1 バイト超えは断る: %v", err)
		}
	})
	t.Run("non-positive-means-default", func(t *testing.T) {
		if _, err := ParseCommands(strings.NewReader(body(repeatCmds(32)...)), Limits{MaxBytes: -1, MaxCommands: -1}); err != nil {
			t.Fatal(err)
		}
	})
}

// countReader は、読んだバイト数を数える。
type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestParseCommandsReadsOnlyWhatItNeeds は、大きさを宣言しただけの pkt-line を、読まずに断る (読み過ぎない) ことを確かめる。
func TestParseCommandsReadsOnlyWhatItNeeds(t *testing.T) {
	big := "fff0" + strings.Repeat("a", 100000)
	cr := &countReader{r: strings.NewReader(big)}
	if _, err := ParseCommands(cr, Limits{}); Reason(err) != CodeTooLarge {
		t.Fatalf("err = %v", err)
	}
	if cr.n != 4 {
		t.Fatalf("宣言だけで断るはずが、%d バイト読んだ", cr.n)
	}
	// 通った後も、コマンド部の終わりを超えて読まない (探りの確認 1 バイトを除く)。
	data := body(repeatCmds(1)...) + "PACK" + strings.Repeat("x", 100000)
	cr = &countReader{r: strings.NewReader(data)}
	push, err := ParseCommands(cr, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if cr.n != len(push.Raw) {
		t.Fatalf("コマンド部は %d バイトなのに、%d バイト読んだ", len(push.Raw), cr.n)
	}
}

// TestParseCommandsTruncation は、正しい本文の、途中までの全ての長さで、通らないことを確かめる。
func TestParseCommandsTruncation(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(capturedD, "*.bin"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		full, err := ParseCommands(bytes.NewReader(data), Limits{})
		if err != nil {
			continue
		}
		for n := 0; n < len(full.Raw); n++ {
			push, err := ParseCommands(bytes.NewReader(data[:n]), Limits{})
			if err == nil || Reason(err) != CodeTruncated {
				t.Fatalf("%s: %d バイトまで: 通った・別の理由 (%v, %+v)", filepath.Base(f), n, err, push)
			}
		}
	}
}

// TestParseCommandsReadError は、読み取りの失敗を、要求の拒否 (*Error) と取り違えないことを確かめる。
func TestParseCommandsReadError(t *testing.T) {
	boom := errors.New("boom")
	full := body(repeatCmds(1)...)
	for _, n := range []int{0, 2, 4, 20, len(full) - 4, len(full) - 1} {
		r := io.MultiReader(strings.NewReader(full[:n]), iotest.ErrReader(boom))
		_, err := ParseCommands(r, Limits{})
		if !errors.Is(err, boom) || Reason(err) != "" {
			t.Fatalf("%d バイト後の失敗: err = %v (Reason %q)", n, err, Reason(err))
		}
	}
	// 探りの確認の読み取りの失敗も、同じ。
	if _, err := ParseCommands(io.MultiReader(strings.NewReader("0000"), iotest.ErrReader(boom)), Limits{}); !errors.Is(err, boom) {
		t.Fatalf("探りの後の失敗: %v", err)
	}
}

// TestErrorsArePrintable は、要求に含まれる制御文字・エスケープシーケンスが、エラーの文字列に通らないことを確かめる。
func TestErrorsArePrintable(t *testing.T) {
	// 攻撃者の文字 (固定の文言に無い、書式制御・別の文字・行区切り・かな漢字) が、そのまま通らないこと。
	const hostile = "\xe2\x80\xae\xe2\x80\x8b\xe2\x80\xa8\xd0\xb6\xe9\xbe\x98\xf0\x9f\x98\x80"
	evil := "\x1b]0;pwned\x07\x1b[2J\r\n\x00" + hostile + strings.Repeat("A", 500)
	bodies := []string{
		body(cmdLine(zero(), oid1, evil)),
		body(cmdLine(zero(), oid1, pfx+"x") + "\x00 " + evil),
		body(cmdLine(zero(), oid1, pfx+"x") + "\x00 agent=" + evil),
		body(evil),
		body("shallow " + evil),
		evil,
	}
	for _, b := range bodies {
		_, err := ParseCommands(strings.NewReader(b), Limits{})
		if err == nil {
			t.Fatalf("通った: %q", b)
		}
		checkInvariants(t, []byte(b), nil, err)
		if strings.ContainsAny(err.Error(), hostile) {
			t.Fatalf("攻撃者の文字が、エラーの文字列に、そのまま入った: %q", err.Error())
		}
	}
	p, _ := NewPolicy(Repo{"o", "r"}, sess)
	_, routeErr := ParseRoute("GET", PathPrefix+evil)
	for _, err := range []error{p.CheckRef(evil), p.CheckRepo(Repo{evil, evil}), CheckBranchName(evil), p.CheckBranch(evil), routeErr} {
		if err == nil || !safeText(err.Error()) || len(err.Error()) > 400 || strings.ContainsAny(err.Error(), hostile) {
			t.Fatalf("エラーの文字列が安全でない: %v", err)
		}
	}
}

// FuzzParseCommands は、任意のバイト列で、パニックせず、通したものは canonical で、上限の内側で、Raw が入力の先頭であることを確かめる。
// 種は、採取した本文と、敵対的な書き方 (testdata/fuzz にも、コミットした種がある)。
func FuzzParseCommands(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join(capturedD, "*.bin"))
	for _, name := range files {
		data, _ := os.ReadFile(name)
		f.Add(data)
	}
	first := cmdLine(zero(), oid1, pfx+"x") + "\x00" + capsFull
	for _, s := range []string{"", "0000", "0000x", "0001", "0004", "fff0", "ffff", pkt(first), body(first), body(first, first), body("shallow " + oid1 + "\n")} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		push, err := ParseCommands(bytes.NewReader(data), Limits{})
		checkInvariants(t, data, push, err)
		if err != nil {
			return
		}
		// 通した Raw だけを読み直しても、同じ結果になる (Raw が、自分で完結している)。
		again, err := ParseCommands(bytes.NewReader(push.Raw), Limits{})
		if err != nil || !reflect.DeepEqual(again, push) {
			t.Fatalf("Raw を読み直すと変わる: %v", err)
		}
		// 1 バイトずつ読ませても、同じ結果になる (読み方に依らない)。
		slow, err := ParseCommands(iotest.OneByteReader(bytes.NewReader(data)), Limits{})
		if err != nil || !reflect.DeepEqual(slow, push) {
			t.Fatalf("1 バイトずつ読むと変わる: %v", err)
		}
	})
}

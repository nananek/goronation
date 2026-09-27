package credential

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// token は、値の目印。出力のどこにも、現れてはならない。
const token = "github_pat_11AAAAAAA0SECRETSECRETSECRET_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// leaks は、out に、値 (全体・一部) が含まれていれば、その理由を返す。
func leaks(out string) string {
	for _, part := range []string{token, "SECRETSECRET", "github_pat_", "11AAAAAAA0", "abcdefghij"} {
		if strings.Contains(out, part) {
			return part
		}
	}
	return ""
}

type holder struct {
	Public string
	tok    Secret // unexported: fmt は、メソッドを呼べず、フィールドを反射で出す
	Tok    Secret
	Ptr    *Secret
}

// Secret は、fmt の全ての動詞・全ての入れ方 (値・ポインタ・slice・map・構造体のフィールド (unexported も)・interface) で、値を出さない。
func TestSecretNeverPrintsViaFmt(t *testing.T) {
	s := New(token)
	h := holder{Public: "p", tok: s, Tok: s, Ptr: &s}
	values := map[string]any{
		"値": s, "ポインタ": &s, "slice": []Secret{s, s}, "ポインタの slice": []*Secret{&s}, "map の値": map[string]Secret{"k": s},
		"構造体": h, "構造体のポインタ": &h, "any": any(s), "配列": [2]Secret{s, s}, "入れ子": struct{ A struct{ B Secret } }{A: struct{ B Secret }{s}},
		"unexported だけ": struct{ tok Secret }{s},
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%p", "%T", "%10s", "%-20v", "%.3s", "%08v", "%U", "%c"}
	for name, v := range values {
		for _, verb := range verbs {
			if got := fmt.Sprintf(verb, v); leaks(got) != "" {
				t.Errorf("%s に %s: 値 %q が出た: %s", name, verb, leaks(got), got)
			}
		}
		for _, got := range []string{fmt.Sprint(v), fmt.Sprintln(v), fmt.Sprint("prefix", v, "suffix")} {
			if leaks(got) != "" {
				t.Errorf("%s に Sprint: 値 %q が出た: %s", name, leaks(got), got)
			}
		}
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "%v|%+v|%#v", v, v, v)
		if leaks(buf.String()) != "" {
			t.Errorf("%s に Fprintf: 値が出た", name)
		}
	}
	if got := fmt.Sprintf("%v", s); got != "[redacted]" {
		t.Errorf("%%v = %q, want [redacted]", got)
	}
	if got := s.String(); got != "[redacted]" || s.GoString() != "[redacted]" {
		t.Errorf("String・GoString = %q・%q", got, s.GoString())
	}
	// unexported のフィールドに入れても、値でなく、アドレスが出る (fmt は、メソッドを呼べない)。
	if got := fmt.Sprintf("%+v", struct{ tok Secret }{s}); strings.Contains(got, "SECRET") {
		t.Errorf("unexported のフィールド: %s", got)
	}
}

// error に包んでも、%w・%v・errors.Join でも、値が出ない。
func TestSecretNeverPrintsInErrors(t *testing.T) {
	s := New(token)
	base := errors.New("boom")
	for name, err := range map[string]error{
		"Errorf %v":   fmt.Errorf("失敗: %v", s),
		"Errorf %s":   fmt.Errorf("失敗: %s", s),
		"Errorf %+v":  fmt.Errorf("失敗: %+v", &s),
		"Errorf %w":   fmt.Errorf("失敗: %w", fmt.Errorf("%v: %w", s, base)),
		"Join":        errors.Join(base, fmt.Errorf("%v", s)),
		"CheckName":   CheckName(strings.ToUpper("BAD NAME")),
		"ErrNotFound": fmt.Errorf("%w: %v", ErrNotFound, s),
	} {
		if leaks(err.Error()) != "" || leaks(fmt.Sprintf("%v %+v %#v", err, err, err)) != "" {
			t.Errorf("%s: error に値が出た: %v", name, err)
		}
	}
}

// panic の値に Secret を渡しても、回復した値の表示・recover 後の fmt に、値が出ない。
func TestSecretNeverPrintsInPanic(t *testing.T) {
	s := New(token)
	for name, v := range map[string]any{"値": s, "ポインタ": &s, "構造体": holder{tok: s, Tok: s}} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s: panic しない", name)
				}
				for _, out := range []string{fmt.Sprint(r), fmt.Sprintf("%v %+v %#v %s", r, r, r, r)} {
					if leaks(out) != "" {
						t.Errorf("%s: panic の値の表示に、値が出た: %s", name, out)
					}
				}
			}()
			panic(v)
		}()
	}
}

// JSON・テキスト・slog は、"[redacted]" だけ (構造体のフィールド・map の値・map のキー・ポインタの中でも)。
func TestSecretNeverPrintsInEncodings(t *testing.T) {
	s := New(token)
	for name, v := range map[string]any{
		"値": s, "ポインタ": &s, "構造体": holder{Public: "p", tok: s, Tok: s, Ptr: &s}, "map の値": map[string]Secret{"k": s},
		"slice": []Secret{s}, "入れ子": map[string]any{"a": []any{s, &s}},
	} {
		b, err := json.Marshal(v)
		if err != nil || leaks(string(b)) != "" {
			t.Errorf("%s: json.Marshal = %s, %v", name, b, err)
		}
		b, err = json.MarshalIndent(v, "", " ")
		if err != nil || leaks(string(b)) != "" {
			t.Errorf("%s: json.MarshalIndent = %s, %v", name, b, err)
		}
	}
	if b, _ := json.Marshal(s); string(b) != `"[redacted]"` {
		t.Errorf("json.Marshal(Secret) = %s", b)
	}
	// map のキー (encoding.TextMarshaler が使われる)。
	if b, err := json.Marshal(map[Secret]int{s: 1}); err != nil || leaks(string(b)) != "" {
		t.Errorf("map のキー: %s, %v", b, err)
	}
	var tm encoding.TextMarshaler = s
	if b, err := tm.MarshalText(); err != nil || string(b) != "[redacted]" {
		t.Errorf("MarshalText = %q, %v", b, err)
	}
	// JSON から戻しても、値は復元されない (Secret は、外から値を受け取る口を持たない)。
	var back Secret
	if err := json.Unmarshal([]byte(`"`+token+`"`), &back); err == nil && back.Reveal() == token {
		t.Error("JSON から、値が復元された")
	}

	for name, h := range map[string]slog.Handler{
		"text": slog.NewTextHandler(new(bytes.Buffer), nil),
		"json": slog.NewJSONHandler(new(bytes.Buffer), nil),
	} {
		var buf bytes.Buffer
		switch name {
		case "text":
			h = slog.NewTextHandler(&buf, nil)
		case "json":
			h = slog.NewJSONHandler(&buf, nil)
		}
		l := slog.New(h)
		l.Info("msg", "tok", s, "ptr", &s, "h", holder{tok: s, Tok: s}, slog.Group("g", "tok", s), slog.Any("any", s))
		l.With("tok", s).WithGroup("grp").Warn("with", "tok", s)
		if leaks(buf.String()) != "" || !strings.Contains(buf.String(), "[redacted]") {
			t.Errorf("slog %s: %s", name, buf.String())
		}
	}
}

// 値は Reveal だけが返す。コピーは同じ値。ゼロ値は空。
func TestSecretReveal(t *testing.T) {
	s := New(token)
	c := s
	if s.Reveal() != token || c.Reveal() != token {
		t.Error("Reveal が、値を返さない")
	}
	var zero Secret
	if zero.Reveal() != "" || !zero.IsZero() || !New("").IsZero() || s.IsZero() {
		t.Errorf("ゼロ値・空・IsZero: %q", zero.Reveal())
	}
	if fmt.Sprint(zero) != "[redacted]" { // ゼロ値も、同じ表示 (空かどうかも、出力からは分からない)
		t.Errorf("ゼロ値の表示 = %q", fmt.Sprint(zero))
	}
	// 値を後から変えても、Secret は変わらない (New は、値を写す)。
	orig := "abc"
	sec := New(orig)
	orig = "changed"
	if sec.Reveal() != "abc" {
		t.Errorf("Reveal = %q", sec.Reveal())
	}
}

// fuzzOutputs は、値 v の Secret を、いろいろな経路で出力した文字列の全部。
func fuzzOutputs(v string) []string {
	s := New(v)
	h := holder{tok: s, Tok: s, Ptr: &s}
	outs := []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x %d", s, s, s, s, s, s, s), fmt.Sprintf("%+v %#v", h, h), fmt.Sprint(&h),
		fmt.Sprint([]Secret{s}), fmt.Sprint(map[string]Secret{"k": s}), fmt.Errorf("e: %w", fmt.Errorf("%v", s)).Error(),
	}
	if b, err := json.Marshal(h); err == nil {
		outs = append(outs, string(b))
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "s", s, "h", h)
	return append(outs, buf.String())
}

func FuzzSecretNeverPrints(f *testing.F) {
	for _, seed := range []string{token, "x", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "日本語のトークン", "a b\nc\x00d", "%s%v%d", "[redacted", "\x1b[31m", "cred"} {
		f.Add(seed)
	}
	// 値と無関係に出る文字列 (型名・フィールド名・slog のキー・アドレスの 16 進) は、値が一致しても、漏れではない: 判定の対象外にする。
	baseline := strings.Join(fuzzOutputs("\x00\x01"), "\n")
	hexOnly := func(v string) bool { return strings.Trim(v, "0123456789abcdefx") == "" }
	f.Fuzz(func(t *testing.T, v string) {
		if len(v) < 3 || strings.Contains(baseline, v) || hexOnly(v) {
			t.Skip()
		}
		for _, out := range fuzzOutputs(v) {
			if strings.Contains(out, v) {
				t.Fatalf("値が出た: 値 %q が %q に含まれる", v, out)
			}
		}
		if New(v).Reveal() != v {
			t.Fatalf("Reveal が、値を返さない: %q", v)
		}
	})
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"github", "a", "vault-1", "a1b2", strings.Repeat("a", 32), "my-cred"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "GitHub", "1abc", "-abc", "a_b", "a.b", "a/b", "../x", "..", ".", "a b", "a\n", "a\x00", strings.Repeat("a", 33), "日本語", "a%2fb", "a:b", "~", "a\\b"} {
		err := CheckName(bad)
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("CheckName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
}

// Source は、名前で引く interface で、無ければ ErrNotFound (errors.Is で、包まれていても判定できる)。
type fakeSource map[string]Secret

func (f fakeSource) Token(ctx context.Context, name string) (Secret, error) {
	if err := CheckName(name); err != nil {
		return Secret{}, err
	}
	if s, ok := f[name]; ok {
		return s, nil
	}
	return Secret{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

func TestSourceContract(t *testing.T) {
	var src Source = fakeSource{"github": New(token)}
	if s, err := src.Token(context.Background(), "github"); err != nil || s.Reveal() != token {
		t.Errorf("Token = %v, %v", s, err)
	}
	if _, err := src.Token(context.Background(), "other"); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidName) {
		t.Errorf("無い名前: %v, want ErrNotFound", err)
	}
	if _, err := src.Token(context.Background(), "../x"); !errors.Is(err, ErrInvalidName) || errors.Is(err, ErrNotFound) {
		t.Errorf("形が違う名前: %v, want ErrInvalidName", err)
	}
	if errors.Is(ErrNotFound, ErrInvalidName) {
		t.Error("ErrNotFound と ErrInvalidName が、同じ")
	}
}

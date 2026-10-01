package v0

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func samplePermission() PermissionRequested {
	return PermissionRequested{
		RequestID: "r1", CallID: "c1", ToolName: "Write", Kind: KindEdit,
		Input: json.RawMessage(`{"file_path":"/work/new.txt","content":"new file\n","n":1.0}`),
		Title: "new.txt", Summary: "Write: /work/new.txt",
		Details: []Detail{{Label: "path", Text: "/work/new.txt", Kind: DetailPath}, {Label: "content", Text: "new file\n"}},
	}
}

func TestPermissionRequestedValidate(t *testing.T) {
	if err := samplePermission().Validate(); err != nil {
		t.Fatal(err)
	}
	// 旧い形 (要約・詳細なし) も、読める。
	old := samplePermission()
	old.Summary, old.Details = "", nil
	if err := old.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(p *PermissionRequested){
		"request_id が空":     func(p *PermissionRequested) { p.RequestID = "" },
		"tool_name が空":      func(p *PermissionRequested) { p.ToolName = "" },
		"input が配列":         func(p *PermissionRequested) { p.Input = json.RawMessage(`[1]`) },
		"input が null":      func(p *PermissionRequested) { p.Input = json.RawMessage(`null`) },
		"summary に改行":       func(p *PermissionRequested) { p.Summary = "a\nb" },
		"summary が長い":       func(p *PermissionRequested) { p.Summary = strings.Repeat("あ", MaxSummaryLen+1) },
		"details が多い":       func(p *PermissionRequested) { p.Details = make([]Detail, MaxDetails+1) },
		"detail の label が空": func(p *PermissionRequested) { p.Details = []Detail{{Label: "", Text: "x"}} },
		"detail の text が長い": func(p *PermissionRequested) {
			p.Details = []Detail{{Label: "x", Text: strings.Repeat("a", MaxDetailTextLen+1)}}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			p := samplePermission()
			mut(&p)
			if err := p.Validate(); err == nil || !errors.Is(err, ErrInvalidPermission) {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}

func TestSummaryLineAndClampText(t *testing.T) {
	if got := SummaryLine("a\r\nb\tc", 20); got != "a  b c" {
		t.Errorf("改行・タブ: %q", got)
	}
	if got := SummaryLine("あいうえお", 5); got != "あいうえお" {
		t.Errorf("ちょうど: %q", got)
	}
	if got := SummaryLine("あいうえおか", 5); got != "あいうえ…" {
		t.Errorf("切る: %q", got)
	}
	// 危険な文字は、ここでは変えない (印にするのは UI)。
	if got := SummaryLine("a\u202eb\x1b[0m", 20); got != "a\u202eb\x1b[0m" {
		t.Errorf("制御文字を変えた: %q", got)
	}
	if s, cut := ClampText("abcdef", 3); s != "abc" || !cut {
		t.Errorf("ClampText = %q %v", s, cut)
	}
	if s, cut := ClampText("abc", 3); s != "abc" || cut {
		t.Errorf("ClampText = %q %v", s, cut)
	}
}

func TestHashIsDeterministicAndSensitive(t *testing.T) {
	p := samplePermission()
	h := p.Hash()
	if !strings.HasPrefix(h, HashPrefix) || len(h) != len(HashPrefix)+64 {
		t.Fatalf("Hash = %q", h)
	}
	// ContentHash 自身は、ハッシュに入らない。
	q := p
	q.ContentHash = "sha256:x"
	if q.Hash() != h {
		t.Error("ContentHash の値が、ハッシュに影響した")
	}
	// input のキーの順序・空白は、ハッシュに影響しない (正規形)。
	r := p
	r.Input = json.RawMessage(`{ "n": 1.0, "content": "new file\n", "file_path": "/work/new.txt" }`)
	if r.Hash() != h {
		t.Error("input のキーの順序・空白で、ハッシュが変わった")
	}
	// 内容が 1 つでも変われば、変わる。
	muts := map[string]func(p *PermissionRequested){
		"request_id": func(p *PermissionRequested) { p.RequestID = "r2" },
		"call_id":    func(p *PermissionRequested) { p.CallID = "c2" },
		"tool_name":  func(p *PermissionRequested) { p.ToolName = "Edit" },
		"kind":       func(p *PermissionRequested) { p.Kind = KindExecute },
		"input の値": func(p *PermissionRequested) {
			p.Input = json.RawMessage(`{"file_path":"/etc/passwd","content":"new file\n","n":1.0}`)
		},
		"input の数": func(p *PermissionRequested) {
			p.Input = json.RawMessage(`{"file_path":"/work/new.txt","content":"new file\n","n":1}`)
		},
		"title":     func(p *PermissionRequested) { p.Title = "other" },
		"summary":   func(p *PermissionRequested) { p.Summary = "Write: /etc/passwd" },
		"details":   func(p *PermissionRequested) { p.Details[1].Text = "evil" },
		"truncated": func(p *PermissionRequested) { p.DetailsTruncated = true },
	}
	for name, mut := range muts {
		c := samplePermission()
		mut(&c)
		if c.Hash() == h {
			t.Errorf("%s を変えても、ハッシュが同じ", name)
		}
	}
	// 型の違う要求 (permission と form) は、同じ欄でも、同じ値にならない (接頭辞の違い)。
	f := sampleForm()
	if f.Hash() == h || f.Hash() == "" {
		t.Error("form のハッシュ")
	}
	f2 := sampleForm()
	f2.Fields[0].Options[0].Label = "Rouge"
	if f2.Hash() == f.Hash() {
		t.Error("form の選択肢を変えても、ハッシュが同じ")
	}
	f3 := f
	f3.Fields = append([]FormField(nil), f.Fields...)
	f3.Fields[0], f3.Fields[1] = f3.Fields[1], f3.Fields[0]
	if f3.Hash() == f.Hash() {
		t.Error("フィールドの順序を変えても、ハッシュが同じ (順序は内容)")
	}
}

func TestVerifyHash(t *testing.T) {
	h := samplePermission().Hash()
	if err := VerifyHash(h, h, true); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHash(h, "", false); err != nil {
		t.Fatalf("導入の途中は、無くてよい: %v", err)
	}
	if err := VerifyHash(h, "", true); !errors.Is(err, ErrHashRequired) {
		t.Fatalf("必須: %v", err)
	}
	if err := VerifyHash(h, "sha256:00", false); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("不一致: %v", err)
	}
	if err := VerifyHash("", "sha256:00", false); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("保持した値が無いのに、応答にある: %v", err)
	}
	// 1 文字違いも拒否する。
	other := h[:len(h)-1] + "0"
	if h[len(h)-1] == '0' {
		other = h[:len(h)-1] + "1"
	}
	if err := VerifyHash(h, other, true); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("1 文字違い: %v", err)
	}
}

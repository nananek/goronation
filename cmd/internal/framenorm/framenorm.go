package framenorm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// 採取したフレームには、実行ごとに変わる値 (UUID・エージェントの ID・時刻・所要時間・費用) が混ざる。
// 同じ場面を再実行して「構造的に同じ」フレーム列になることを確かめるため、NormalizeFrames は、それらを
// 固定の記号に置き換える。
//
//   - ID (UUID・opencode の ses_/msg_/prt_ 形式) は、最初に現れた順の番号を付けた記号 ("<uuid:1>"・
//     "<msg:2>") にする。同じ ID は同じ記号になるので、フレーム間の参照関係 (同じ session・message) は残る。
//   - 値が数値・文字列の時刻・所要時間・費用のキー (下の isVolatileKey) は、数値なら 0、文字列なら
//     "<キー名>" にする。opencode の "time" (開始・終了のミリ秒) は、中の数値をすべて 0 にする。
//
// 値の型・キーの有無・フレームの順序は変えない。JSON のキーの順序は、Go の encoding/json に従い、
// キー名の辞書順になる (元の出力の順序は保たない)。
var (
	uuidRe       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	opencodeIDRe = regexp.MustCompile(`^(ses|msg|prt)_[0-9A-Za-z]+$`)
)

// volatileKeys は、値が実行ごとに変わるキー (どの深さでも)。
var volatileKeys = map[string]bool{
	"timestamp":       true,
	"date":            true, // opencode のエラーの responseHeaders.date
	"duration_ms":     true,
	"duration_api_ms": true,
	"total_cost_usd":  true,
	"costUSD":         true,
	// claude の init フレームの、実行ごとに番号が変わる UDS の path。
	"messaging_socket_path": true,
	// PR⓪ の対話採取: control_response (initialize の応答) に載る、檻の中での claude の実際の OS pid。
	"pid": true,
}

// isVolatileKey は、key の値が実行ごとに変わるか。volatileKeys のほか、"_ms" で終わるキー
// (duration_ms・ttft_ms・time_to_request_ms など、所要時間のミリ秒) も含む。
func isVolatileKey(key string) bool {
	return volatileKeys[key] || strings.HasSuffix(key, "_ms")
}

// Rules は、既定の規則 (上) に足す規則。ゼロ値は、既定のまま (cmd/framecapture の既存の golden は、これで正規化される)。
type Rules struct {
	// VolatileKeys は、既定に足す、値が実行ごとに変わるキー (どの深さでも)。
	VolatileKeys map[string]bool
	// IDRe に合う文字列は、"<接頭辞:N>" にする (接頭辞は、最初の "_" の前)。nil なら、既定 (ses_・msg_・prt_ だけ)。
	IDRe *regexp.Regexp
	// HashRe に合う文字列は、"<hash:N>" にする (パスから決まる識別子など)。nil なら、しない。
	HashRe *regexp.Regexp
	// TextIDRe は、文字列の中に埋まった ID (別の文字列の一部。パス・本文中) の規則: 合った部分を、IDRe と同じ記号 (同じ番号の体系) にする。nil なら、しない。
	TextIDRe *regexp.Regexp
	// Text は、文字列の中の一部 (ポートなど) を置き換える規則 (上の規則のあとに、順に適用する)。
	Text []TextRule
}

// TextRule は、文字列の値の中の、Re に合う部分を Repl に置き換える。
type TextRule struct {
	Re   *regexp.Regexp
	Repl string
}

// Serve は、opencode 2.x の serve (HTTP + SSE) の採取の規則: イベント・許可・form などの ID (evt_・per_・frm_ …)・イベントの作成時刻 (created)・
// session の slug (毎回違う名前)・パスから決まる 40 桁の 16 進の識別子 (projectID)。
var Serve = Rules{
	VolatileKeys: map[string]bool{"created": true, "slug": true, "started": true, "ended": true, "at": true},
	IDRe:         regexp.MustCompile(`^[a-z]{2,4}_[0-9A-Za-z]{20,}$`),
	TextIDRe:     regexp.MustCompile(`\b[a-z]{2,4}_[0-9A-Za-z]{20,}`),
	HashRe:       regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`),
	Text: []TextRule{
		{regexp.MustCompile(`127\.0\.0\.1:\d+`), "127.0.0.1:<port>"},
		{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{20,}`), "<cursor>"}, // ページ送りの cursor (base64url の JSON。中に message の ID が入る)
	},
}

// Normalizer は、ID の番号付けの状態 (記号の種類ごとの、最初に現れた順の番号)。同じ Normalizer に続けて渡した行は、同じ番号の体系になる。
type Normalizer struct {
	rules Rules
	next  map[string]int    // 記号の種類 → 次の番号
	sym   map[string]string // 元の ID → 記号
}

// New は、規則 r の Normalizer。
func New(r Rules) *Normalizer {
	return &Normalizer{rules: r, next: map[string]int{}, sym: map[string]string{}}
}

func (n *Normalizer) volatile(key string) bool {
	return isVolatileKey(key) || n.rules.VolatileKeys[key]
}

// Symbol は、文字列 s が ID なら、その記号を返す (ID でなければ、s のまま)。パスの 1 区間などに使う。
func (n *Normalizer) Symbol(s string) string {
	return n.value("", s, false).(string)
}

// idSymbol は、ID id (種類 kind) の記号 ("<kind:N>") を返す。
func (n *Normalizer) idSymbol(kind, id string) string {
	if s, ok := n.sym[id]; ok {
		return s
	}
	n.next[kind]++
	s := fmt.Sprintf("<%s:%d>", kind, n.next[kind])
	n.sym[id] = s
	return s
}

// value は、v (json.Number を使って decode した値) を、key の下の値として正規化する。inTime は、
// "time" オブジェクトの下にいること。
func (n *Normalizer) value(key string, v any, inTime bool) any {
	switch x := v.(type) {
	case map[string]any:
		// キー名の順に辿る (ID の番号を、map の走査順に依らず決めるため)。
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			nk := k
			if n.rules.IDRe != nil && n.rules.IDRe.MatchString(k) { // ID が、キーになっているもの
				nk = n.idSymbol(k[:strings.IndexByte(k, '_')], k)
			}
			out[nk] = n.value(k, x[k], inTime || k == "time")
		}
		return out
	case []any:
		for i, child := range x {
			x[i] = n.value(key, child, inTime)
		}
		return x
	case json.Number:
		if inTime || n.volatile(key) {
			return json.Number("0")
		}
		return x
	case string:
		switch {
		case n.volatile(key):
			return "<" + key + ">"
		case uuidRe.MatchString(x):
			return n.idSymbol("uuid", x)
		case opencodeIDRe.MatchString(x) || (n.rules.IDRe != nil && n.rules.IDRe.MatchString(x)):
			return n.idSymbol(x[:strings.IndexByte(x, '_')], x)
		case n.rules.HashRe != nil && n.rules.HashRe.MatchString(x):
			return n.idSymbol("hash", x)
		}
		if re := n.rules.TextIDRe; re != nil {
			x = re.ReplaceAllStringFunc(x, func(m string) string { return n.idSymbol(m[:strings.IndexByte(m, '_')], m) })
		}
		for _, r := range n.rules.Text {
			x = r.Re.ReplaceAllString(x, r.Repl)
		}
		return x
	}
	return v
}

// NormalizeFrames は、raw (1 行 1 JSON の列。空行は読み飛ばす) の各行を、既定の規則で正規化して、同じ形 (1 行 1 JSON) で返す。
func NormalizeFrames(raw []byte) ([]byte, error) {
	return New(Rules{}).Frames(raw)
}

// Frames は、raw (1 行 1 JSON の列。空行は読み飛ばす) の各行を正規化して、同じ形 (1 行 1 JSON) で返す。
func (n *Normalizer) Frames(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	for i, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		b, err := n.Line(line)
		if err != nil {
			return nil, fmt.Errorf("%d 行目が JSON でない: %w", i+1, err)
		}
		out.Write(b)
	}
	return out.Bytes(), nil
}

// Line は、1 つの JSON を正規化して、末尾に改行を付けて返す。
func (n *Normalizer) Line(line []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out) // 末尾の改行を付ける
	enc.SetEscapeHTML(false)
	if err := enc.Encode(n.value("", v, false)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

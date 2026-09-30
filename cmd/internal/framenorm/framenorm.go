// Package framenorm は、採取したフレーム (1 行 1 JSON) の、実行ごとに変わる値の正規化を持つ (cmd/framecapture と、serve の採取 (cmd/goronation の test) が共有する)。
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

// normalizer は、ID の番号付けの状態 (記号の種類ごとの、最初に現れた順の番号)。
type normalizer struct {
	next map[string]int    // 記号の種類 → 次の番号
	sym  map[string]string // 元の ID → 記号
}

func newNormalizer() *normalizer {
	return &normalizer{next: map[string]int{}, sym: map[string]string{}}
}

// idSymbol は、ID id (種類 kind) の記号 ("<kind:N>") を返す。
func (n *normalizer) idSymbol(kind, id string) string {
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
func (n *normalizer) value(key string, v any, inTime bool) any {
	switch x := v.(type) {
	case map[string]any:
		// キー名の順に辿る (ID の番号を、map の走査順に依らず決めるため)。
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			x[k] = n.value(k, x[k], inTime || k == "time")
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = n.value(key, child, inTime)
		}
		return x
	case json.Number:
		if inTime || isVolatileKey(key) {
			return json.Number("0")
		}
		return x
	case string:
		switch {
		case isVolatileKey(key):
			return "<" + key + ">"
		case uuidRe.MatchString(x):
			return n.idSymbol("uuid", x)
		case opencodeIDRe.MatchString(x):
			return n.idSymbol(x[:strings.IndexByte(x, '_')], x)
		}
		return x
	}
	return v
}

// NormalizeFrames は、raw (1 行 1 JSON の列。空行は読み飛ばす) の各行を正規化して、同じ形 (1 行 1 JSON) で返す。
func NormalizeFrames(raw []byte) ([]byte, error) {
	n := newNormalizer()
	var out bytes.Buffer
	for i, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%d 行目が JSON でない: %w", i+1, err)
		}
		enc := json.NewEncoder(&out) // 1 回ごとに末尾の改行を付ける
		enc.SetEscapeHTML(false)
		if err := enc.Encode(n.value("", v, false)); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

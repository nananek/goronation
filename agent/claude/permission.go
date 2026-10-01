package claude

import (
	"bytes"
	"encoding/json"
	"slices"
	"unicode/utf8"

	v0 "github.com/nananek/goronation/spec/v0"
)

// 権限の要求の要約と詳細 (ADR 0041)。承認する対象 (tool の名前と input) から、機械的に作る。input は書き換えない (updatedInput の元になる)。
//
// 承認するのは input の全体なので、詳細は、input の最上位のキーを全部持つ: 既知の tool は、決まった項目 (command・path など) を先に、
// そのほかのキー (Bash の dangerouslyDisableSandbox・Edit の replace_all など、実行に効くもの) を、続けて項目にする。
// 知らない tool は、全てのキーを辞書順に項目にする。項目の数・長さの上限を超えるときは、切って DetailsTruncated を立てる (UI は承認させない)。
// label は、既知の項目では固定の語で、そのほかの項目ではエージェントが決めたキー (UI は、エージェントの文として、印を通して表示する)。

// detailSpec は、既知の tool の、詳細の 1 項目 (input のキー → label・kind)。
type detailSpec struct{ label, key, kind string }

// knownTools は、既知の tool の要約の主な値のキーと、詳細の項目。
var knownTools = map[string]struct {
	summaryKey string
	details    []detailSpec
}{
	"Bash":     {"command", []detailSpec{{"command", "command", v0.DetailCommand}}},
	"Write":    {"file_path", []detailSpec{{"path", "file_path", v0.DetailPath}, {"content", "content", v0.DetailText}}},
	"Edit":     {"file_path", []detailSpec{{"path", "file_path", v0.DetailPath}, {"old_string", "old_string", v0.DetailText}, {"new_string", "new_string", v0.DetailText}}},
	"Read":     {"file_path", []detailSpec{{"path", "file_path", v0.DetailPath}}},
	"WebFetch": {"url", []detailSpec{{"url", "url", v0.DetailURL}, {"prompt", "prompt", v0.DetailText}}},
}

// buildPermission は、can_use_tool の要求から、PermissionRequested (ContentHash なし。共通の層が付ける) を作る。input は、オブジェクトであること (呼び手が確かめる)。
func buildPermission(id, callID, name string, input json.RawMessage, title string) v0.PermissionRequested {
	p := v0.PermissionRequested{RequestID: id, CallID: callID, ToolName: name, Kind: toolKind(name), Input: bytes.Clone(input), Title: title}
	var in map[string]json.RawMessage
	_ = json.Unmarshal(input, &in) // オブジェクトであることは、呼び手が確かめた

	truncated := false
	add := func(label, text, kind string) {
		if len(p.Details) >= v0.MaxDetails { // 項目が多すぎる: 見せていないものがある
			truncated = true
			return
		}
		if label == "" {
			label = "(空のキー)"
		}
		if utf8.RuneCountInString(label) > v0.MaxDetailLabelLen {
			label, truncated = string([]rune(label)[:v0.MaxDetailLabelLen]), true
		}
		text, cut := v0.ClampText(text, v0.MaxDetailTextLen)
		truncated = truncated || cut
		p.Details = append(p.Details, v0.Detail{Label: label, Text: text, Kind: kind})
	}

	used := map[string]bool{}
	if k, ok := knownTools[name]; ok {
		p.Summary = v0.SummaryLine(name+": "+valueText(in[k.summaryKey]), v0.MaxSummaryLen)
		for _, d := range k.details {
			used[d.key] = true
			add(d.label, valueText(in[d.key]), d.kind)
		}
	} else {
		p.Summary = v0.SummaryLine(name, v0.MaxSummaryLen)
	}
	keys := make([]string, 0, len(in))
	for key := range in {
		if !used[key] {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		add(key, valueText(in[key]), v0.DetailText)
	}
	p.DetailsTruncated = truncated
	return p
}

// valueText は、input の値を、人が読む文字列にする: 文字列はそのまま、ほかは JSON の文字列 (入れ子も、そのまま 1 つの文字列)。無い値は空。
func valueText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

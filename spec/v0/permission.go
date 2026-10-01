package v0

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 権限の要求の 2 階層の語彙 (ADR 0041): 要約 (機械生成の 1 行) と、詳細 (tool に依らない、ラベルつきの項目の一覧)。
// 要約と詳細は、承認する対象 (tool の名前と input) から、アダプタが機械的に作る。エージェントの自己申告 (Title) とは別で、Title は
// 検証されていない説明として、別に表示する。input は、書き換えず、そのまま保持する。

// 詳細の項目の kind。UI は、kind で表示の形 (等幅・折り返し) を変えてよい。知らない kind は text と同じ。
const (
	DetailText    = "text"
	DetailCommand = "command"
	DetailPath    = "path"
	DetailURL     = "url"
)

// 上限。長さは文字数 (rune)。
const (
	MaxSummaryLen     = 200
	MaxDetails        = 16
	MaxDetailLabelLen = 64
	// MaxDetailTextLen は、詳細の 1 項目の本文の上限 (ADR 0016 決定 8 の「見せていない input は許可させない」の、見せる上限と同じ 4,000)。
	// 超えるときは、アダプタが切り、DetailsTruncated を true にする。
	MaxDetailTextLen = 4000
	MaxToolNameLen   = 128
)

// Detail は、詳細の 1 項目。Label は固定の語 (アダプタが決める。エージェントの文ではない)。Text は、承認する対象の値で、
// エージェントが決めた値を含むので、UI は、危険な文字を印にして表示する (ADR 0015)。
type Detail struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	Kind  string `json:"kind,omitempty"`
}

// PermissionRequested は、TypePermissionRequested の data。ADR 0041 以前の欄 (request_id・call_id・tool_name・kind・input・title) は
// 変わらない。Summary・Details・DetailsTruncated・ContentHash は、追加の欄で、無くても後方互換に読める (UI は、無ければ input の表示に戻る)。
type PermissionRequested struct {
	RequestID string          `json:"request_id"`
	CallID    string          `json:"call_id,omitempty"`
	ToolName  string          `json:"tool_name"`
	Kind      string          `json:"kind"`
	Input     json.RawMessage `json:"input"`
	// Title は、エージェントの自己申告の説明 (検証されていない)。
	Title string `json:"title,omitempty"`
	// Summary は、人間が読める 1 行 (改行を含まない・MaxSummaryLen 以内)。
	Summary string   `json:"summary,omitempty"`
	Details []Detail `json:"details,omitempty"`
	// DetailsTruncated が true のとき、詳細は、承認する対象の全体でない (切ってある)。UI は、承認させない (見せていないものを承認させない)。
	DetailsTruncated bool `json:"details_truncated,omitempty"`
	// ContentHash は、この要求の内容の SHA-256 (Hash の値)。goronation の共通の層が付ける。
	ContentHash string `json:"content_hash,omitempty"`
}

// PermissionResolve は、CommandPermissionResolve の data。ContentHash は、利用者が見た要求の ContentHash の写し (ADR 0042)。
type PermissionResolve struct {
	RequestID   string `json:"request_id"`
	Outcome     string `json:"outcome"`
	ContentHash string `json:"content_hash,omitempty"`
}

// ErrInvalidPermission は、形の不正。
var ErrInvalidPermission = errors.New("v0: invalid permission request")

func invalidPermission(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidPermission}, a...)...)
}

// Validate は、要求の形を検査する。要約・詳細は無くてもよい (旧い形)。あれば、上限と、Summary が 1 行であることを検査する。
func (p PermissionRequested) Validate() error {
	if p.RequestID == "" || p.ToolName == "" || utf8.RuneCountInString(p.ToolName) > MaxToolNameLen {
		return invalidPermission("request_id・tool_name")
	}
	if !isJSONObject(p.Input) {
		return invalidPermission("input がオブジェクトでない")
	}
	if n := utf8.RuneCountInString(p.Summary); n > MaxSummaryLen || strings.ContainsAny(p.Summary, "\r\n") {
		return invalidPermission("summary が 1 行でない・長い")
	}
	if len(p.Details) > MaxDetails {
		return invalidPermission("details の数 %d", len(p.Details))
	}
	for i, d := range p.Details {
		if d.Label == "" || utf8.RuneCountInString(d.Label) > MaxDetailLabelLen || utf8.RuneCountInString(d.Text) > MaxDetailTextLen {
			return invalidPermission("details[%d] の label・text", i)
		}
	}
	return nil
}

func isJSONObject(b json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

// SummaryLine は、s を、要約の 1 行にする: 改行・タブを空白 1 つにし、max 文字を超えたら、切って末尾に「…」を付ける。
// 危険な文字 (制御文字・双方向制御) は、ここでは変えない: 印にして表示するのは UI の仕事 (ADR 0015。同じ入力を、UI が、いつも同じ規則で見せる)。
func SummaryLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// ClampText は、s を max 文字以内にする。切ったかを返す (詳細の項目の本文。切ったら、DetailsTruncated を立てる)。
func ClampText(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	return string([]rune(s)[:max]), true
}

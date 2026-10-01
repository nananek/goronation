package opencode

import (
	"bytes"
	"encoding/json"
	"strings"

	v0 "github.com/nananek/goronation/spec/v0"
)

// permissionAsked は、permission.asked の data のうち、変換が読む部分。
type permissionAsked struct {
	ID        lenient[string]   `json:"id"`
	SessionID lenient[string]   `json:"sessionID"`
	Action    lenient[string]   `json:"action"`
	Resources lenient[[]string] `json:"resources"`
	Source    lenient[struct {
		ID lenient[string] `json:"id"`
	}] `json:"source"`
	Metadata json.RawMessage `json:"metadata"`
}

// permissionInput は、承認する対象 (v0.PermissionRequested.Input)。id・sessionID・source は経路の情報で、request_id・call_id に写す
// (input に入れない)。save は always の保存の対象で、v0 は always を出さない。metadata は、書き換えずに載せる。
type permissionInput struct {
	Action    string          `json:"action"`
	Resources []string        `json:"resources"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type permissionMetadata struct {
	Files lenient[[]struct {
		File  lenient[string] `json:"file"`
		Patch lenient[string] `json:"patch"`
	}] `json:"files"`
	URL    lenient[string] `json:"url"`
	Query  lenient[string] `json:"query"`
	Root   lenient[string] `json:"root"`
	Format lenient[string] `json:"format"`
}

// permissionAsked は、permission.asked を permission.requested にする (ADR 0041・0043)。action: question の要求も、通常の承認として扱う。
// 形が不正・上限超過の要求は、承認できない (要求を保持しない)。opencode は応答を待ち続けるので、黙って捨てず TypeError で知らせる。
func (s *Stream) permissionAsked(o *out, data json.RawMessage) {
	var d permissionAsked
	_ = json.Unmarshal(data, &d)
	origin, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad {
		o.frame()
		return
	}
	id := d.ID.V
	if d.ID.Bad || !validID(id) || d.Action.Bad || d.Action.V == "" || d.Resources.Bad {
		o.frame()
		o.add(v0.TypeError, true, nil, errorData(invalidMessage))
		return
	}
	key := seenKey("p", id)
	if _, dup := s.seen[key]; dup { // 同じ ID の再要求は、決着済みでも捨てる (先の要求の内容を、後から差し替えさせない)
		o.frame()
		return
	}
	if len(s.pending)+len(s.forms) >= maxPending || len(s.seen) >= maxSeen {
		o.add(v0.TypeError, true, nil, errorData(limitMessage))
		return
	}
	req, truncated := buildPermission(d)
	if err := req.Validate(); err != nil {
		o.frame()
		o.add(v0.TypeError, true, nil, errorData(invalidMessage))
		return
	}
	if s.pending == nil {
		s.pending = map[string]pendingPerm{}
	}
	if s.seen == nil { // form が先に来ていると、seen は既にある (作り直すと、見た ID を失う)
		s.seen = map[[32]byte]uint8{}
	}
	s.seen[key] = stRequested
	s.nextSeq++
	s.pending[id] = pendingPerm{sid: d.SessionID.V, seq: s.nextSeq, truncated: truncated, callID: req.CallID}
	o.add(v0.TypePermissionRequested, true, origin, req)
}

func errorData(msg string) map[string]any {
	return map[string]any{"status": nil, "retryable": false, "message": msg}
}

// buildPermission は、承認する対象 (action・resources・metadata) から、要約と詳細を機械的に作る (ADR 0041)。詳細が全体を持てないとき
// (項目が 16 を超える・1 項目が 4,000 字を超える) は、切って DetailsTruncated を立てる (UI は承認させない。EncodeCommand も allow を断る)。
func buildPermission(d permissionAsked) (v0.PermissionRequested, bool) {
	action, resources := d.Action.V, d.Resources.V
	if resources == nil {
		resources = []string{}
	}
	in := permissionInput{Action: action, Resources: resources}
	var md permissionMetadata
	if isObject(d.Metadata) {
		in.Metadata = d.Metadata
		_ = json.Unmarshal(d.Metadata, &md)
	}
	inputJSON, _ := marshal(in) // 固定の構造体 (RawMessage は、検証済みの JSON)
	req := v0.PermissionRequested{
		RequestID: d.ID.V, CallID: d.Source.V.ID.V, ToolName: action, Kind: toolKind(action), Input: inputJSON,
	}
	sep, label, dk := ", ", "target", v0.DetailText
	switch action {
	case "shell":
		sep, label, dk = " ; ", "command", v0.DetailCommand // 部分コマンドの一覧。1 つずつ見せる (ADR 0021 決定 4)
	case "edit", "write", "read", "external_directory":
		label, dk = "path", v0.DetailPath
	case "glob", "grep":
		label = "pattern"
	case "webfetch":
		label, dk = "url", v0.DetailURL
	case "websearch":
		label = "query"
	}
	req.Summary = v0.SummaryLine(action+": "+strings.Join(resources, sep), v0.MaxSummaryLen)
	truncated := false
	add := func(l, text, k string) {
		if len(req.Details) >= v0.MaxDetails {
			truncated = true
			return
		}
		l, labelCut := v0.ClampText(l, v0.MaxDetailLabelLen) // label も値 (patch: <file>) を含む。切れたら、見せていない部分がある
		t, cut := v0.ClampText(text, v0.MaxDetailTextLen)
		truncated = truncated || cut || labelCut
		req.Details = append(req.Details, v0.Detail{Label: l, Text: t, Kind: k})
	}
	for _, r := range resources {
		add(label, r, dk)
	}
	if !md.Files.Bad { // edit・write: 変更の差分 (patch) も見せる
		for _, f := range md.Files.V {
			add("patch: "+f.File.V, f.Patch.V, v0.DetailText)
		}
	}
	for _, kv := range []struct {
		k string
		v lenient[string]
	}{{"url", md.URL}, {"query", md.Query}, {"root", md.Root}, {"format", md.Format}} {
		if kv.v.V != "" && !contains(resources, kv.v.V) {
			add(kv.k, kv.v.V, v0.DetailText)
		}
	}
	// 詳細に出していない欄は、承認する対象の一部を見せていないことになる (ADR 0041 決定 3)。見えない欄に束縛された承認はさせない
	// (拒否はできる)。未観測の action・版の更新で増える欄も、ここで fail closed になる。見せるものが何も無い要求も、同じ。
	if len(resources) == 0 || hiddenMetadata(d.Metadata) {
		truncated = true
	}
	req.DetailsTruncated = truncated
	return req, truncated
}

// hiddenMetadata は、metadata に、buildPermission が詳細に出さない欄 (または読めない形) があるか。
func hiddenMetadata(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return true
	}
	for k, v := range m {
		switch k {
		case "url", "query", "root", "format":
			var str string
			if json.Unmarshal(v, &str) != nil {
				return true
			}
		case "files":
			var fs []map[string]json.RawMessage
			if json.Unmarshal(v, &fs) != nil {
				return true
			}
			for _, f := range fs {
				for fk, fv := range f {
					// file・patch は詳細に出す文字列。additions・deletions・status は patch の集計で、patch から分かる (採取した edit の files は
					// 全てこの形)。型・長さを検査しないと、見えない場所に自由な値を持てる (詳細に出す側は、型違いを空にする)。
					switch fk {
					case "file", "patch":
						var str string
						if json.Unmarshal(fv, &str) != nil {
							return true
						}
					case "additions", "deletions":
						var n float64
						if json.Unmarshal(fv, &n) != nil {
							return true
						}
					case "status":
						var str string
						if json.Unmarshal(fv, &str) != nil || len(str) > 32 {
							return true
						}
					default:
						return true
					}
				}
			}
		default:
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// permissionReplied は、permission.replied。EncodeCommand が返したもの (合成の permission.resolved (by=human) がある) は出さない。
// 未決のまま来た replied (別のクライアント・opencode の側の決着) は by=agent。それ以外 (未知の ID・決着済み) は agent.frame。
func (s *Stream) permissionReplied(o *out, data json.RawMessage) {
	var d struct {
		RequestID lenient[string] `json:"requestID"`
		SessionID lenient[string] `json:"sessionID"`
		Reply     lenient[string] `json:"reply"`
	}
	_ = json.Unmarshal(data, &d)
	id := d.RequestID.V
	if d.RequestID.Bad || d.SessionID.Bad || !validID(id) {
		o.frame()
		return
	}
	if p, ok := s.pending[id]; ok {
		if p.sid != d.SessionID.V {
			o.frame()
			return
		}
		delete(s.pending, id)
		s.seen[seenKey("p", id)] = stByAgent
		origin, _ := s.scope(p.sid)
		o.add(v0.TypePermissionResolved, true, origin, map[string]any{"by": "agent", "outcome": replyOutcome(d.Reply.V), "request_id": id})
		return
	}
	if s.seen[seenKey("p", id)] == stByUs {
		return
	}
	o.frame()
}

func replyOutcome(reply string) string {
	switch reply {
	case "once":
		return v0.AllowOnce
	case "always":
		return v0.AllowAlways
	case "reject":
		return v0.RejectOnce
	}
	return outcomeCancelled
}

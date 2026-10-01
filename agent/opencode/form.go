package opencode

import (
	"encoding/json"

	v0 "github.com/nananek/goronation/spec/v0"
)

// formCreated は、form.created の data.form のうち、変換が読む部分。
type formCreated struct {
	ID        lenient[string] `json:"id"`
	SessionID lenient[string] `json:"sessionID"`
	Title     lenient[string] `json:"title"`
	Metadata  lenient[struct {
		Kind lenient[string] `json:"kind"`
		Tool lenient[struct {
			ID lenient[string] `json:"id"`
		}] `json:"tool"`
	}] `json:"metadata"`
	Fields lenient[[]struct {
		Key         lenient[string] `json:"key"`
		Title       lenient[string] `json:"title"`
		Description lenient[string] `json:"description"`
		Type        lenient[string] `json:"type"`
		Custom      lenient[bool]   `json:"custom"`
		Required    lenient[bool]   `json:"required"`
		Options     lenient[[]struct {
			Label       lenient[string] `json:"label"`
			Value       lenient[string] `json:"value"`
			Description lenient[string] `json:"description"`
		}] `json:"options"`
	}] `json:"fields"`
}

// formCreated は、form.created を form.requested にする (ADR 0040)。形が不正・上限超過の form は、応答できない (保持しない)。
// opencode は応答を待ち続けるので、黙って捨てず TypeError で知らせる。key・option の value は、応答で返す値なので、書き換えず保持する。
func (s *Stream) formCreated(o *out, data json.RawMessage) {
	var d struct {
		Form lenient[formCreated] `json:"form"`
	}
	_ = json.Unmarshal(data, &d)
	f := d.Form.V
	origin, ok := s.scope(f.SessionID.V)
	if d.Form.Bad || !ok || f.SessionID.Bad {
		o.frame()
		return
	}
	id := f.ID.V
	if f.ID.Bad || !validID(id) {
		o.frame()
		o.add(v0.TypeError, true, nil, errorData(invalidMessage))
		return
	}
	key := seenKey("f", id)
	if _, dup := s.seen[key]; dup {
		o.frame()
		return
	}
	if len(s.pending)+len(s.forms) >= maxPending || len(s.seen) >= maxSeen {
		o.add(v0.TypeError, true, nil, errorData(limitMessage))
		return
	}
	req, ok := buildForm(f)
	if !ok || req.Validate() != nil {
		o.frame()
		o.add(v0.TypeError, true, nil, errorData(invalidMessage))
		return
	}
	if s.forms == nil {
		s.forms = map[string]pendingForm{}
	}
	if s.seen == nil {
		s.seen = map[[32]byte]uint8{}
	}
	s.seen[key] = stRequested
	s.nextSeq++
	s.forms[id] = pendingForm{sid: f.SessionID.V, seq: s.nextSeq, req: req}
	o.add(v0.TypeFormRequested, true, origin, req)
}

// buildForm は、opencode の form を FormRequested にする。知らない field の type は、応答の形が決められないので、ok=false。
func buildForm(f formCreated) (v0.FormRequested, bool) {
	req := v0.FormRequested{RequestID: f.ID.V, Kind: "other", Title: f.Title.V}
	if !f.Metadata.Bad {
		if k := f.Metadata.V.Kind.V; !f.Metadata.V.Kind.Bad && safeKind(k) {
			req.Kind = k
		}
		if !f.Metadata.V.Tool.Bad {
			req.CallID = f.Metadata.V.Tool.V.ID.V
		}
	}
	if f.Fields.Bad {
		return req, false
	}
	for _, fo := range f.Fields.V {
		fd := v0.FormField{Key: fo.Key.V, Title: fo.Title.V, Description: fo.Description.V, Custom: fo.Custom.V, Required: fo.Required.V}
		if fo.Type.Bad || fo.Options.Bad { // type・options が、あるのに読めない field は、応答の形が決められない (key は、読めなければ空で、Validate が断る)
			return req, false
		}
		switch fo.Type.V {
		case "multiselect":
			fd.Type = v0.FieldMultiselect
		case "string":
			fd.Type = v0.FieldSelect
			if len(fo.Options.V) == 0 {
				fd.Type = v0.FieldText
			}
		default:
			return req, false
		}
		for _, op := range fo.Options.V {
			val := op.Value.V
			if val == "" {
				val = op.Label.V // エージェントが値を出さないときは、Label (FormOption の doc)
			}
			fd.Options = append(fd.Options, v0.FormOption{Label: op.Label.V, Value: val, Description: op.Description.V})
		}
		req.Fields = append(req.Fields, fd)
	}
	return req, true
}

// safeKind は、metadata.kind として、そのまま載せてよい文字列か ([A-Za-z0-9._-] の 1〜64 文字)。
func safeKind(k string) bool {
	if k == "" || len(k) > v0.MaxFormKindLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// formReplied は、form.replied。EncodeCommand が返したもの (合成の form.resolved (by=human) がある) は出さない。
// 未決のまま来たもの (別のクライアント) は by=agent・answered。それ以外は agent.frame。
func (s *Stream) formReplied(o *out, data json.RawMessage) {
	var d struct {
		ID        lenient[string] `json:"id"`
		SessionID lenient[string] `json:"sessionID"`
		Answer    json.RawMessage `json:"answer"`
	}
	_ = json.Unmarshal(data, &d)
	s.formSettledBySSE(o, d.ID, d.SessionID, func(m map[string]any) {
		m["outcome"] = v0.FormAnswered
		var ans map[string]v0.FormValue
		if json.Unmarshal(d.Answer, &ans) == nil && ans != nil { // 読めない回答は、載せない (決着の事実だけ)
			m["answer"] = ans
		}
	})
}

// formCancelled は、form.cancelled。
func (s *Stream) formCancelled(o *out, data json.RawMessage) {
	var d struct {
		ID        lenient[string] `json:"id"`
		SessionID lenient[string] `json:"sessionID"`
	}
	_ = json.Unmarshal(data, &d)
	s.formSettledBySSE(o, d.ID, d.SessionID, func(m map[string]any) { m["outcome"] = v0.FormCancelled })
}

func (s *Stream) formSettledBySSE(o *out, id, sid lenient[string], fill func(map[string]any)) {
	if id.Bad || sid.Bad || !validID(id.V) {
		o.frame()
		return
	}
	if p, ok := s.forms[id.V]; ok {
		if p.sid != sid.V {
			o.frame()
			return
		}
		delete(s.forms, id.V)
		s.seen[seenKey("f", id.V)] = stByAgent
		origin, _ := s.scope(p.sid)
		m := map[string]any{"by": "agent", "request_id": id.V}
		fill(m)
		o.add(v0.TypeFormResolved, true, origin, m)
		return
	}
	if s.seen[seenKey("f", id.V)] == stByUs {
		return
	}
	o.frame()
}

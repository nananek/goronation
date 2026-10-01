package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	v0 "github.com/nananek/goronation/spec/v0"
)

// httpRequest は、EncodeCommand が返す、HTTP 要求の記述 (ADR 0022 決定 3。transport が実行する)。body が無い要求は null。
type httpRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

// EncodeCommand は、Command を、HTTP 要求の記述 ({"method","path","body"} の JSON 1 行。改行つき) にする。
//   - prompt: POST /api/session/{root}/prompt。合成の turn.started を返す。
//   - permission.resolve: POST /api/session/{sid}/permission/{id}/reply ({"decision":"once"|"reject"})。sid は、要求が出た session
//     (子の session の要求は、子の ID)。合成の permission.resolved (by=human) を返す。allow_always・reject_always は error。
//   - form.resolve: answered は POST …/form/{id}/reply ({"answer":{…}})、cancelled は DELETE …/form/{id}。合成の form.resolved (by=human)。
//   - cancel: POST /api/session/{root}/interrupt (結果は SSE の execution.interrupted)。
//
// 未決でない ID (未知・応答済み・失効済み) は error で、何も書かない (1 回限り)。Command.Data の未知の欄は無視する。
func (s *Stream) EncodeCommand(cmd v0.Command) ([]byte, []v0.Envelope, error) {
	switch cmd.Type {
	case v0.CommandPrompt:
		return s.encodePrompt(cmd)
	case v0.CommandCancel:
		return s.encodeCancel()
	case v0.CommandPermissionResolve:
		return s.encodePermissionResolve(cmd)
	case v0.CommandFormResolve:
		return s.encodeFormResolve(cmd)
	}
	return nil, nil, fmt.Errorf("opencode: 未対応のコマンド %q", cmd.Type)
}

// sessionPath は、/api/session/{sid} (+ 固定の語 + 検査済みの ID)。ID は、validID を通ったものだけ (path の区切り・.. ・制御文字を通さない)。
func sessionPath(sid string, rest ...string) (string, error) {
	if !validID(sid) {
		return "", errors.New("opencode: session の ID が不正")
	}
	p := "/api/session/" + url.PathEscape(sid)
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p, nil
}

func idPath(sid, word, id, tail string) (string, error) {
	if !validID(id) {
		return "", errors.New("opencode: 要求の ID が不正")
	}
	if tail == "" {
		return sessionPath(sid, word, id)
	}
	return sessionPath(sid, word, id, tail)
}

func encodeRequest(method, path string, body any) ([]byte, error) {
	r := httpRequest{Method: method, Path: path}
	if body != nil {
		b, err := marshal(body)
		if err != nil {
			return nil, err
		}
		r.Body = b
	}
	b, err := marshal(r)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func synth(typ string, origin *v0.Origin, data any) (v0.Envelope, error) {
	b, err := marshal(data)
	if err != nil {
		return v0.Envelope{}, err
	}
	return v0.Envelope{V: v0.Version, Type: typ, Durable: true, Origin: origin, Data: b}, nil
}

func (s *Stream) rootPath(rest ...string) (string, error) {
	if s.root == "" {
		return "", errors.New("opencode: root session が未確定 (session.created を、まだ見ていない)")
	}
	return sessionPath(s.root, rest...)
}

func (s *Stream) encodePrompt(cmd v0.Command) ([]byte, []v0.Envelope, error) {
	var d struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(cmd.Data, &d); err != nil {
		return nil, nil, fmt.Errorf("opencode: prompt の data が読めない: %w", err)
	}
	if d.Text == "" {
		return nil, nil, errors.New("opencode: prompt の text が空")
	}
	path, err := s.rootPath("prompt")
	if err != nil {
		return nil, nil, err
	}
	line, err := encodeRequest("POST", path, map[string]string{"text": d.Text})
	if err != nil {
		return nil, nil, err
	}
	started, err := synth(v0.TypeTurnStarted, nil, map[string]any{"text": d.Text})
	if err != nil {
		return nil, nil, err
	}
	return line, []v0.Envelope{started}, nil
}

func (s *Stream) encodeCancel() ([]byte, []v0.Envelope, error) {
	path, err := s.rootPath("interrupt")
	if err != nil {
		return nil, nil, err
	}
	line, err := encodeRequest("POST", path, nil)
	return line, nil, err
}

func (s *Stream) encodePermissionResolve(cmd v0.Command) ([]byte, []v0.Envelope, error) {
	var d struct {
		RequestID string `json:"request_id"`
		Outcome   string `json:"outcome"`
	}
	if err := json.Unmarshal(cmd.Data, &d); err != nil {
		return nil, nil, fmt.Errorf("opencode: permission.resolve の data が読めない: %w", err)
	}
	var decision string
	switch d.Outcome {
	case v0.AllowOnce:
		decision = "once"
	case v0.RejectOnce:
		decision = "reject"
	default: // allow_always・reject_always・未知の値 (ADR 0021 決定 3: once・reject だけ)
		return nil, nil, fmt.Errorf("opencode: 未対応の outcome %q", d.Outcome)
	}
	p, ok := s.pending[d.RequestID]
	if !ok {
		return nil, nil, fmt.Errorf("opencode: 未決でない request_id %q", d.RequestID)
	}
	if decision == "once" && p.truncated { // 見せていないものを承認させない (ADR 0016 決定 8・0041 決定 3)。拒否はできる
		return nil, nil, errors.New("opencode: 詳細が切れている要求は、許可できない (拒否はできる)")
	}
	path, err := idPath(p.sid, "permission", d.RequestID, "reply")
	if err != nil {
		return nil, nil, err
	}
	line, err := encodeRequest("POST", path, map[string]string{"decision": decision})
	if err != nil {
		return nil, nil, err
	}
	origin, _ := s.scope(p.sid)
	resolved, err := synth(v0.TypePermissionResolved, origin, map[string]any{"by": "human", "outcome": d.Outcome, "request_id": d.RequestID})
	if err != nil {
		return nil, nil, err
	}
	delete(s.pending, d.RequestID)
	s.seen[seenKey("p", d.RequestID)] = stByUs
	return line, []v0.Envelope{resolved}, nil
}

func (s *Stream) encodeFormResolve(cmd v0.Command) ([]byte, []v0.Envelope, error) {
	var r v0.FormResolve
	if err := json.Unmarshal(cmd.Data, &r); err != nil {
		return nil, nil, fmt.Errorf("opencode: form.resolve の data が読めない: %w", err)
	}
	p, ok := s.forms[r.RequestID]
	if !ok {
		return nil, nil, fmt.Errorf("opencode: 未決でない request_id %q", r.RequestID)
	}
	// 回答は、要求時に保持した form (key・options・型・必須) だけに対して検査する。Conversation の検査と二重の防御。
	if err := r.Validate(p.req); err != nil {
		return nil, nil, fmt.Errorf("opencode: form の回答が不正: %w", err)
	}
	path, err := idPath(p.sid, "form", r.RequestID, "")
	if err != nil {
		return nil, nil, err
	}
	data := map[string]any{"by": "human", "outcome": r.Outcome, "request_id": r.RequestID}
	var line []byte
	switch r.Outcome {
	case v0.FormAnswered:
		ans := r.Answer
		if ans == nil {
			ans = map[string]v0.FormValue{}
		}
		if line, err = encodeRequest("POST", path+"/reply", map[string]any{"answer": ans}); err != nil {
			return nil, nil, err
		}
		data["answer"] = ans
	default: // FormCancelled (Validate が、それ以外の outcome を断っている)
		if line, err = encodeRequest("DELETE", path, nil); err != nil {
			return nil, nil, err
		}
	}
	origin, _ := s.scope(p.sid)
	resolved, err := synth(v0.TypeFormResolved, origin, data)
	if err != nil {
		return nil, nil, err
	}
	delete(s.forms, r.RequestID)
	s.seen[seenKey("f", r.RequestID)] = stByUs
	return line, []v0.Envelope{resolved}, nil
}

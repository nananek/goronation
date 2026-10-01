package opencode

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nananek/goronation/core/agent"
	v0 "github.com/nananek/goronation/spec/v0"
)

// Name は、このアダプタの名前 (agent.Adapter.Name)。
const Name = "opencode"

// TestedVersion は、SSE の形を採取した opencode の版 (spec/testdata/golden/opencode-serve)。版が上がると形が変わりうるので、
// 未知の type・形は agent.frame (または承認できない側) に倒す (lenient。ADR 0022 決定 6)。
const TestedVersion = "2.0.20"

var _ agent.Adapter = Adapter{}

// Adapter は、opencode (serve の HTTP + SSE) の agent.Adapter。
type Adapter struct{}

// Name は、"opencode"。
func (Adapter) Name() string { return Name }

// NewStream は、1 回の起動 (1 つの root session) に紐づく Stream を作る。
func (Adapter) NewStream() agent.Stream { return &Stream{} }

// 上限。超えた要求は、承認・応答できない側に倒す (誤って許可はしない)。
const (
	maxPending     = 64      // 未決の permission・form の合計
	maxSeen        = 1 << 17 // 見た要求・form の ID (SHA-256 で覚える。回復しない)
	maxChildren    = 1024    // 子 session (サブエージェント) の数 (累計。終わった子も、後のイベントのため覚える)
	maxCalls       = 4096    // 覚える tool 名の数
	maxToolName    = 128
	maxErrorText   = 4000 // error の message (文字数)
	limitMessage   = "opencode: 権限要求・form の上限に達したため、この要求は承認できない (opencode は応答を待ち続けている)"
	invalidMessage = "opencode: 形が不正な権限要求・form は、承認できない (opencode は応答を待ち続けている)"
)

// 要求の状態 (seen の値)。決着の後も覚え、同じ ID の再要求を新しい要求にしない。
const (
	stRequested = iota + 1
	stByUs      // EncodeCommand が返した (SSE の replied・cancelled は、合成済みの決着と重複するので出さない)
	stByAgent   // 失効・別のクライアントの返答で閉じた
)

// child は、子 session (サブエージェント)。callID は、起動した tool 呼び出し (分かれば)。
type child struct {
	parent string
	callID string
}

type pendingPerm struct {
	sid       string
	seq       uint64
	truncated bool // 詳細が切れている (見せていないものは承認させない)
	callID    string
}

type pendingForm struct {
	sid string
	seq uint64
	req v0.FormRequested // 応答の検査は、要求時に保持したこの値だけに対して行う
}

// Stream は、1 つの opencode の起動 (root session とその子) の、状態を持つ変換器。並行には呼べない。
//
// 入力は SSE の data: の JSON 1 行。root は、最初の parentID の無い session.created で、以後の EncodeCommand が使う。root でも
// 子でもない session のイベントは、agent.frame にする (ほかの利用者の session)。
//
// 承認フローの完全性は、SSE の出どころが opencode 自身であることに依る (ADR 0010・0022 と同じ限界)。ID は opencode が振る値だが、
// agent.frame に落とさず通せるのは validID の形だけ。
type Stream struct {
	root     string
	children map[string]child

	calls map[string]string // sid + "\x00" + call id → tool 名

	pending map[string]pendingPerm
	forms   map[string]pendingForm
	seen    map[[sha256.Size]byte]uint8
	nextSeq uint64
}

func seenKey(prefix, id string) [sha256.Size]byte { return sha256.Sum256([]byte(prefix + "\x00" + id)) }

// out は、DecodeFrame が返す Envelope の組み立て。
type out struct {
	line []byte
	list []v0.Envelope
	err  error
}

func (o *out) add(typ string, durable bool, origin *v0.Origin, data any) {
	if o.err != nil {
		return
	}
	b, err := marshal(data)
	if err != nil {
		o.err = err
		return
	}
	o.list = append(o.list, v0.Envelope{V: v0.Version, Type: typ, Durable: durable, Origin: origin, Data: b, Raw: o.line})
}

func (o *out) frame() { o.add(v0.TypeAgentFrame, false, nil, struct{}{}) }

func (o *out) done() ([]v0.Envelope, error) {
	if o.err != nil {
		return nil, o.err
	}
	return o.list, nil
}

// scope は、session ID sid の帰属を返す: root は (nil, true)・子は Origin つき・どちらでもなければ ok=false。
func (s *Stream) scope(sid string) (*v0.Origin, bool) {
	if sid == "" || s.root == "" {
		return nil, false
	}
	if sid == s.root {
		return nil, true
	}
	c, ok := s.children[sid]
	if !ok {
		return nil, false
	}
	return &v0.Origin{ID: sid, Parent: c.callID}, true
}

// DecodeFrame は、SSE の data: の JSON 1 行を、0 個以上の Envelope にする。ID・TS・Session・Seq は空。
func (s *Stream) DecodeFrame(raw []byte) ([]v0.Envelope, error) {
	var f event
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("opencode: フレームが JSON でない: %w", err)
	}
	o := &out{line: bytes.Clone(raw)}
	k, known := eventKinds[f.Type.V]
	if f.Type.Bad || !known || k == kFrame {
		o.frame()
		return o.done()
	}
	switch k {
	case kHousekeeping:
	case kSessionCreated:
		s.sessionCreated(o, f.Data)
	case kToolInputStarted:
		s.toolInputStarted(f.Data)
	case kTextEnded:
		s.textEnded(o, f.Data)
	case kToolCalled:
		s.toolCalled(o, f.Data)
	case kToolProgress:
		s.toolProgress(o, f.Data)
	case kToolSuccess:
		s.toolSuccess(o, f.Data)
	case kToolFailed:
		s.toolFailed(o, f.Data)
	case kStepEnded:
		s.stepEnded(o, f.Data)
	case kPermissionAsked:
		s.permissionAsked(o, f.Data)
	case kPermissionReplied:
		s.permissionReplied(o, f.Data)
	case kFormCreated:
		s.formCreated(o, f.Data)
	case kFormReplied:
		s.formReplied(o, f.Data)
	case kFormCancelled:
		s.formCancelled(o, f.Data)
	case kExecSucceeded, kExecFailed, kExecInterrupted:
		s.executionEnded(o, k, f.Data)
	}
	return o.done()
}

type sessionCreated struct {
	SessionID lenient[string] `json:"sessionID"`
	ParentID  lenient[string] `json:"parentID"`
	Version   lenient[string] `json:"version"`
	Location  lenient[struct {
		Directory lenient[string] `json:"directory"`
	}] `json:"location"`
}

func (s *Stream) sessionCreated(o *out, data json.RawMessage) {
	var d sessionCreated
	_ = json.Unmarshal(data, &d)
	sid := d.SessionID
	if sid.Bad || !validID(sid.V) || d.ParentID.Bad {
		o.frame()
		return
	}
	if d.ParentID.V != "" { // 子 (サブエージェント): 追跡だけ。親が root か既知の子のときだけ
		_, parentKnown := s.scope(d.ParentID.V)
		_, dup := s.scope(sid.V)
		if !parentKnown || dup || len(s.children) >= maxChildren || !validID(d.ParentID.V) {
			o.frame()
			return
		}
		if s.children == nil {
			s.children = map[string]child{}
		}
		s.children[sid.V] = child{parent: d.ParentID.V}
		return
	}
	if s.root != "" { // 2 つ目の root は、ほかの利用者の session
		o.frame()
		return
	}
	s.root = sid.V
	cwd := ""
	if !d.Location.Bad {
		cwd = d.Location.V.Directory.V
	}
	o.add(v0.TypeSessionStarted, true, nil, map[string]any{
		"agent": Name, "agent_session": sid.V, "cwd": cwd, "version": clampRunes(d.Version.V, 64),
	})
}

type toolEvent struct {
	SessionID lenient[string] `json:"sessionID"`
	ID        lenient[string] `json:"id"`
	Name      lenient[string] `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   lenient[[]struct {
		Type lenient[string] `json:"type"`
		Text lenient[string] `json:"text"`
	}] `json:"content"`
	Metadata lenient[struct {
		SessionID lenient[string] `json:"sessionID"`
		Exit      lenient[*int]   `json:"exit"`
	}] `json:"metadata"`
	Error lenient[struct {
		Message lenient[string] `json:"message"`
	}] `json:"error"`
}

func callKey(sid, id string) string { return sid + "\x00" + id }

func (s *Stream) toolInputStarted(data json.RawMessage) {
	var d toolEvent
	_ = json.Unmarshal(data, &d)
	if _, ok := s.scope(d.SessionID.V); !ok || d.SessionID.Bad || d.ID.Bad || d.Name.Bad || d.ID.V == "" || len(d.Name.V) > maxToolName {
		return
	}
	if len(s.calls) >= maxCalls {
		return // 覚えない (名前が分からない tool は kind=other になる)
	}
	if s.calls == nil {
		s.calls = map[string]string{}
	}
	s.calls[callKey(d.SessionID.V, d.ID.V)] = d.Name.V
}

func (s *Stream) toolCalled(o *out, data json.RawMessage) {
	var d toolEvent
	_ = json.Unmarshal(data, &d)
	origin, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad || d.ID.Bad || d.ID.V == "" || !isObject(d.Input) {
		o.frame()
		return
	}
	name := s.calls[callKey(d.SessionID.V, d.ID.V)]
	o.add(v0.TypeToolCall, true, origin, map[string]any{
		"call_id": d.ID.V, "name": name, "kind": toolKind(name), "input": orNull(d.Input), "status": v0.ToolPending,
	})
}

func (s *Stream) toolProgress(o *out, data json.RawMessage) {
	var d toolEvent
	_ = json.Unmarshal(data, &d)
	origin, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad || d.ID.Bad || d.ID.V == "" {
		o.frame()
		return
	}
	// 子 session を起動した tool 呼び出し: 子の parent が、このイベントの session のときだけ対応づける (別の session の子を、横取りさせない)。
	if !d.Metadata.Bad {
		if c, ok := s.children[d.Metadata.V.SessionID.V]; ok && c.parent == d.SessionID.V && c.callID == "" {
			c.callID = d.ID.V
			s.children[d.Metadata.V.SessionID.V] = c
		}
	}
	o.add(v0.TypeToolUpdate, true, origin, map[string]any{"call_id": d.ID.V, "status": v0.ToolInProgress})
}

func (s *Stream) toolSuccess(o *out, data json.RawMessage) {
	var d toolEvent
	_ = json.Unmarshal(data, &d)
	origin, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad || d.ID.Bad || d.ID.V == "" {
		o.frame()
		return
	}
	var text strings.Builder
	if !d.Content.Bad {
		for _, c := range d.Content.V {
			if c.Type.V == "text" {
				text.WriteString(c.Text.V)
			}
		}
	}
	m := map[string]any{"call_id": d.ID.V, "status": v0.ToolCompleted, "output": text.String()}
	if !d.Metadata.Bad && !d.Metadata.V.Exit.Bad && d.Metadata.V.Exit.V != nil {
		m["exit"] = *d.Metadata.V.Exit.V
	}
	delete(s.calls, callKey(d.SessionID.V, d.ID.V)) // tool 名は、完了するまで (覚える数を、進行中の呼び出しに比例させる)
	o.add(v0.TypeToolUpdate, true, origin, m)
}

func (s *Stream) toolFailed(o *out, data json.RawMessage) {
	var d toolEvent
	_ = json.Unmarshal(data, &d)
	origin, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad || d.ID.Bad || d.ID.V == "" {
		o.frame()
		return
	}
	msg := ""
	if !d.Error.Bad {
		msg = d.Error.V.Message.V
	}
	delete(s.calls, callKey(d.SessionID.V, d.ID.V))
	o.add(v0.TypeToolUpdate, true, origin, map[string]any{"call_id": d.ID.V, "status": v0.ToolFailed, "error": msg})
}

func (s *Stream) textEnded(o *out, data json.RawMessage) {
	var d struct {
		SessionID lenient[string] `json:"sessionID"`
		MessageID lenient[string] `json:"assistantMessageID"`
		Text      lenient[string] `json:"text"`
	}
	_ = json.Unmarshal(data, &d)
	origin, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad {
		o.frame()
		return
	}
	if d.Text.V == "" {
		return
	}
	o.add(v0.TypeMessageText, true, origin, map[string]any{"message_id": d.MessageID.V, "text": d.Text.V})
}

type tokens struct {
	Input     lenient[int64] `json:"input"`
	Output    lenient[int64] `json:"output"`
	Reasoning lenient[int64] `json:"reasoning"`
	Cache     lenient[struct {
		Read  lenient[int64] `json:"read"`
		Write lenient[int64] `json:"write"`
	}] `json:"cache"`
}

func nonNeg(v int64) int64 { return max(v, 0) }

func (s *Stream) stepEnded(o *out, data json.RawMessage) {
	var d struct {
		SessionID lenient[string]  `json:"sessionID"`
		Cost      lenient[float64] `json:"cost"`
		Tokens    lenient[tokens]  `json:"tokens"`
	}
	_ = json.Unmarshal(data, &d)
	origin, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad {
		o.frame()
		return
	}
	t := d.Tokens.V
	var cacheRead, cacheWrite int64
	if !t.Cache.Bad {
		cacheRead, cacheWrite = t.Cache.V.Read.V, t.Cache.V.Write.V
	}
	o.add(v0.TypeUsage, true, origin, map[string]any{
		"scope":                       "step",
		"input_tokens":                nonNeg(t.Input.V),
		"output_tokens":               nonNeg(t.Output.V),
		"reasoning_tokens":            nonNeg(t.Reasoning.V),
		"cache_read_input_tokens":     nonNeg(cacheRead),
		"cache_creation_input_tokens": nonNeg(cacheWrite),
		"cost_usd":                    max(d.Cost.V, 0),
	})
}

// executionEnded は、session.execution.{succeeded,failed,interrupted}。root の終わりだけが turn.completed (ADR 0045 決定 4。
// 子の終わりは出さない)。終わった session で未決の要求・form は、もう答えられないので、先に by=agent・cancelled で閉じる。
func (s *Stream) executionEnded(o *out, k kind, data json.RawMessage) {
	var d struct {
		SessionID lenient[string] `json:"sessionID"`
		Reason    lenient[string] `json:"reason"`
		Error     lenient[struct {
			Message lenient[string] `json:"message"`
			Status  lenient[*int]   `json:"status"`
		}] `json:"error"`
	}
	_ = json.Unmarshal(data, &d)
	_, ok := s.scope(d.SessionID.V)
	if !ok || d.SessionID.Bad {
		o.frame()
		return
	}
	isRoot := d.SessionID.V == s.root
	s.closePending(o, func(sid string) bool { return isRoot || sid == d.SessionID.V })
	if !isRoot {
		return
	}
	switch k {
	case kExecSucceeded:
		o.add(v0.TypeTurnCompleted, true, nil, map[string]any{"stop_reason": v0.StopEndTurn, "is_error": false})
	case kExecInterrupted:
		reason := d.Reason.V
		if d.Reason.Bad || len(reason) > 32 || !validID(reason) {
			reason = ""
		}
		o.add(v0.TypeTurnCompleted, true, nil, map[string]any{"stop_reason": v0.StopCancelled, "is_error": false, "reason": reason})
	case kExecFailed:
		var status *int
		msg := ""
		if !d.Error.Bad {
			if !d.Error.V.Status.Bad {
				status = d.Error.V.Status.V
			}
			msg = clampRunes(d.Error.V.Message.V, maxErrorText) // url・ヘッダ・response.body は載せない (raw にだけ残る)
		}
		o.add(v0.TypeError, true, nil, map[string]any{"status": status, "retryable": retryable(status), "message": msg})
		o.add(v0.TypeTurnCompleted, true, nil, map[string]any{"stop_reason": v0.StopError, "is_error": true})
	}
}

// closePending は、match する session の未決の要求・form を、届いた順に by=agent・cancelled で閉じる。
func (s *Stream) closePending(o *out, match func(sid string) bool) {
	type entry struct {
		seq    uint64
		isForm bool
		id     string
		sid    string
	}
	var es []entry
	for id, p := range s.pending {
		if match(p.sid) {
			es = append(es, entry{p.seq, false, id, p.sid})
		}
	}
	for id, p := range s.forms {
		if match(p.sid) {
			es = append(es, entry{p.seq, true, id, p.sid})
		}
	}
	slices.SortFunc(es, func(a, b entry) int { return cmp.Compare(a.seq, b.seq) })
	for _, e := range es {
		origin, _ := s.scope(e.sid)
		if e.isForm {
			delete(s.forms, e.id)
			s.seen[seenKey("f", e.id)] = stByAgent
			o.add(v0.TypeFormResolved, true, origin, map[string]any{"by": "agent", "outcome": v0.FormCancelled, "request_id": e.id})
		} else {
			delete(s.pending, e.id)
			s.seen[seenKey("p", e.id)] = stByAgent
			o.add(v0.TypePermissionResolved, true, origin, map[string]any{"by": "agent", "outcome": outcomeCancelled, "request_id": e.id})
		}
	}
}

// outcomeCancelled は、by=agent の失効の outcome (v0.PermissionOptionKind ではない拡張。agent/claude と同じ)。
const outcomeCancelled = "cancelled"

// retryable は、HTTP の status が、再試行して直りうるもの (429・5xx) か。status が無ければ false。
func retryable(status *int) bool {
	return status != nil && (*status == 429 || *status >= 500)
}

// clampRunes は、s を max 文字以内にする (超えたら、切って「…」を付ける)。
func clampRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

// toolKind は、opencode の tool 名・permission の action を、v0 の ToolKind にする。対応表に無いものは other。
func toolKind(name string) string {
	switch name {
	case "shell", "execute":
		return v0.KindExecute
	case "edit", "write":
		return v0.KindEdit
	case "read", "external_directory":
		return v0.KindRead
	case "glob", "grep", "websearch":
		return v0.KindSearch
	case "webfetch":
		return v0.KindFetch
	}
	return v0.KindOther
}

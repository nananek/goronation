package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nananek/goronation/core/agent"
	v0 "github.com/nananek/goronation/spec/v0"
)

// Name は、このアダプタの名前 (agent.Adapter.Name)。
const Name = "claude"

var _ agent.Adapter = Adapter{}

// Adapter は、Claude Code の agent.Adapter。
type Adapter struct{}

// Name は、"claude"。
func (Adapter) Name() string { return Name }

// NewStream は、1 回の起動に紐づく Stream を作る。
func (Adapter) NewStream() agent.Stream { return &Stream{} }

// Stream は、1 回の claude の起動の、状態を持つ変換器。並行には呼べない。
type Stream struct {
	sawInit bool // system/init を、もう見たか (ターンごとに繰り返し出るので、2 回目からは TypeSessionStarted にしない)
}

// lenient は、想定外の型の値を、エラーにせず零値にする。frame を 1 回で読むので、1 つのフィールドの型の不一致で
// Unmarshal 全体が失敗すると、フレームが raw ごと失われる (そのターンの error・usage・turn.completed も)。
type lenient[T any] struct{ V T }

func (l *lenient[T]) UnmarshalJSON(b []byte) error {
	var v T
	if json.Unmarshal(b, &v) == nil {
		l.V = v
	}
	return nil
}

// frame は、claude のフレームのうち、変換が読む部分。読まないフィールドは、宣言しない。
// 型が想定と違うフィールドは、零値として読む (lenient)。
type frame struct {
	Type    lenient[string] `json:"type"`
	Subtype lenient[string] `json:"subtype"`

	// system/init
	SessionID lenient[string]   `json:"session_id"`
	Cwd       lenient[string]   `json:"cwd"`
	Model     lenient[string]   `json:"model"`
	Tools     lenient[[]string] `json:"tools"`

	// assistant・user
	IsAPIErrorMessage lenient[bool] `json:"is_api_error_message"`
	// message は、assistant・user ではオブジェクト、system/permission_denied では文字列なので、型を見てから読む (msg)。
	Message json.RawMessage `json:"message"`

	// system/permission_denied
	ToolName  lenient[string] `json:"tool_name"`
	ToolUseID lenient[string] `json:"tool_use_id"`

	// result
	IsError        lenient[bool]                  `json:"is_error"`
	Result         lenient[string]                `json:"result"`
	APIErrorStatus lenient[*int]                  `json:"api_error_status"`
	TerminalReason lenient[string]                `json:"terminal_reason"`
	StopReason     lenient[string]                `json:"stop_reason"`
	TotalCostUSD   lenient[float64]               `json:"total_cost_usd"`
	Usage          lenient[usage]                 `json:"usage"`
	ModelUsage     lenient[map[string]modelUsage] `json:"modelUsage"`
}

// message は、assistant・user のフレームの message。
type message struct {
	ID      string  `json:"id"`
	Content []block `json:"content"`
}

// msg は、f.Message を、オブジェクトとして読む。
// オブジェクトでなければ ok=false (呼び手は、捨てずに TypeAgentFrame にする。error にすると、フレームが raw ごと失われる)。
func (f frame) msg() (m message, ok bool) {
	return m, json.Unmarshal(f.Message, &m) == nil
}

// block は、message.content の 1 要素 (text・tool_use・tool_result)。
type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type modelUsage struct {
	ContextWindow   int64 `json:"contextWindow"`
	MaxOutputTokens int64 `json:"maxOutputTokens"`
}

// DecodeFrame は、claude の出力の 1 行を、0 個以上の Envelope にする。ID・TS・Session・Seq は空。
func (s *Stream) DecodeFrame(raw []byte) ([]v0.Envelope, error) {
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("claude: フレームが JSON でない: %w", err)
	}
	line := bytes.Clone(raw)
	ev := func(typ string, durable bool, data any) (v0.Envelope, error) {
		b, err := marshal(data)
		if err != nil {
			return v0.Envelope{}, err
		}
		return v0.Envelope{V: v0.Version, Type: typ, Durable: durable, Data: b, Raw: line}, nil
	}
	unknown := func() ([]v0.Envelope, error) {
		e, err := ev(v0.TypeAgentFrame, false, struct{}{})
		return []v0.Envelope{e}, err
	}

	switch {
	case f.Type.V == "system" && f.Subtype.V == "init":
		if s.sawInit {
			return unknown()
		}
		s.sawInit = true
		e, err := ev(v0.TypeSessionStarted, true, map[string]any{
			"agent":         Name,
			"agent_session": f.SessionID.V,
			"cwd":           f.Cwd.V,
			"model":         f.Model.V,
			"tools":         f.Tools.V,
		})
		return []v0.Envelope{e}, err

	case f.Type.V == "assistant" && f.IsAPIErrorMessage.V:
		// 失敗の本文は、同じ内容を持つ後続の result (api_error_status も、そちらにだけある) から TypeError にする。二重に出さない。
		return unknown()

	case f.Type.V == "assistant":
		m, ok := f.msg()
		if !ok {
			return unknown()
		}
		var out []v0.Envelope
		for _, b := range m.Content {
			var e v0.Envelope
			var err error
			switch b.Type {
			case "text":
				e, err = ev(v0.TypeMessageText, true, map[string]any{"message_id": m.ID, "text": b.Text})
			case "tool_use":
				e, err = ev(v0.TypeToolCall, true, map[string]any{
					"call_id": b.ID, "name": b.Name, "kind": toolKind(b.Name), "input": orNull(b.Input), "status": v0.ToolInProgress,
				})
			default: // thinking など (未採取)
				e, err = ev(v0.TypeAgentFrame, false, struct{}{})
			}
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		if len(out) == 0 {
			return unknown()
		}
		return out, nil

	case f.Type.V == "system" && f.Subtype.V == "permission_denied":
		// 非対話の拒否 (claude -p は、要求のフレームを出さず、拒否の結果だけを出す)。同じ事実を指す tool_result の is_error・
		// tool_result_meta・result の permission_denials からは、二重に出さない。message (文) は、path を含むので data に載せない。
		e, err := ev(v0.TypePermissionResolved, true, map[string]any{
			"by": "policy", "outcome": v0.RejectOnce, "call_id": f.ToolUseID.V, "tool_name": f.ToolName.V,
		})
		return []v0.Envelope{e}, err

	case f.Type.V == "user":
		m, ok := f.msg()
		if !ok {
			return unknown()
		}
		var out []v0.Envelope
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				continue
			}
			d := map[string]any{"call_id": b.ToolUseID, "status": v0.ToolCompleted}
			if b.IsError {
				d["status"] = v0.ToolFailed
				d["error"] = orNull(b.Content)
			} else {
				d["output"] = orNull(b.Content)
			}
			e, err := ev(v0.TypeToolUpdate, true, d)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		if len(out) == 0 {
			return unknown()
		}
		return out, nil

	case f.Type.V == "result":
		var window, maxOut int64
		for _, m := range f.ModelUsage.V { // モデルが複数なら、大きい方 (窓の上限を、小さく見せない)
			window, maxOut = max(window, m.ContextWindow), max(maxOut, m.MaxOutputTokens)
		}
		u, err := ev(v0.TypeUsage, true, map[string]any{
			"scope":                       "turn",
			"input_tokens":                f.Usage.V.InputTokens,
			"output_tokens":               f.Usage.V.OutputTokens,
			"cache_read_input_tokens":     f.Usage.V.CacheReadInputTokens,
			"cache_creation_input_tokens": f.Usage.V.CacheCreationInputTokens,
			"cost_usd":                    f.TotalCostUSD.V,
			"context_window":              window,
			"max_output_tokens":           maxOut,
		})
		if err != nil {
			return nil, err
		}
		if f.IsError.V { // subtype は、失敗でも success になるので見ない
			msg := f.Result.V
			if msg == "" {
				msg = f.TerminalReason.V
			}
			// url・ヘッダは載せない (raw にだけ残る)。message は claude が result に整えた文をそのまま載せる。実機では
			// API の応答の本文と request_id を含みうる (fixture は fake の 400 で、この形は未採取)。UI が出す前提の値なので、要約や切り詰めはしない。
			x, err := ev(v0.TypeError, true, map[string]any{"status": f.APIErrorStatus.V, "retryable": retryable(f.APIErrorStatus.V), "message": msg})
			if err != nil {
				return nil, err
			}
			t, err := ev(v0.TypeTurnCompleted, true, map[string]any{"stop_reason": v0.StopError, "is_error": true})
			if err != nil {
				return nil, err
			}
			return []v0.Envelope{x, u, t}, nil
		}
		t, err := ev(v0.TypeTurnCompleted, true, map[string]any{"stop_reason": stopReason(f.StopReason.V), "is_error": false})
		if err != nil {
			return nil, err
		}
		return []v0.Envelope{u, t}, nil
	}
	return unknown()
}

// EncodeCommand は、Command を、claude の標準入力に書く 1 行 (改行を含む) にする。
// prompt は、フレームの無い TypeTurnStarted を、合成して返す。
func (s *Stream) EncodeCommand(cmd v0.Command) ([]byte, []v0.Envelope, error) {
	if cmd.Type != v0.CommandPrompt {
		return nil, nil, fmt.Errorf("claude: 未対応のコマンド %q", cmd.Type)
	}
	var d struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(cmd.Data, &d); err != nil {
		return nil, nil, fmt.Errorf("claude: prompt の data が読めない: %w", err)
	}
	if d.Text == "" {
		return nil, nil, errors.New("claude: prompt の text が空")
	}
	line, err := marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": d.Text}},
		},
	})
	if err != nil {
		return nil, nil, err
	}
	started := v0.Envelope{V: v0.Version, Type: v0.TypeTurnStarted, Durable: true, Data: json.RawMessage(`{}`)}
	return append(line, '\n'), []v0.Envelope{started}, nil
}

// marshal は、HTML 用のエスケープ (< を < にする) をしない json.Marshal。
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// orNull は、フレームに無かった (空の) 値を、JSON の null にする (空の RawMessage は、Marshal が失敗する)。
func orNull(m json.RawMessage) json.RawMessage {
	if len(m) == 0 {
		return json.RawMessage("null")
	}
	return m
}

// retryable は、HTTP の status が、再試行して直りうるもの (429・5xx) か。status が無い (HTTP の失敗でない) 場合は false。
func retryable(status *int) bool {
	return status != nil && (*status == 429 || *status >= 500)
}

// stopReason は、claude の result.stop_reason を、v0 の StopReason にする。
// end_turn 以外の正常終了 (stop_sequence・tool_use など) と未知の値は、end_turn とする (元の値は raw に残る)。
func stopReason(s string) string {
	switch s {
	case v0.StopMaxTokens, v0.StopRefusal:
		return s
	}
	return v0.StopEndTurn
}

// toolKind は、claude の tool の名前を、v0 の ToolKind にする。対応表に無い名前 (MCP の tool・Task・スケジュール系など) は other。
// 名前は、採取した system/init の tools と、claude の組み込みの Grep・Glob・MultiEdit から。
func toolKind(name string) string {
	switch name {
	case "Read":
		return v0.KindRead
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return v0.KindEdit
	case "Grep", "Glob", "WebSearch":
		return v0.KindSearch
	case "Bash":
		return v0.KindExecute
	case "WebFetch":
		return v0.KindFetch
	}
	return v0.KindOther
}

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

// frame は、claude のフレームのうち、変換が読む部分。読まないフィールドは、宣言しない。
type frame struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	SessionID string   `json:"session_id"`
	Cwd       string   `json:"cwd"`
	Model     string   `json:"model"`
	Tools     []string `json:"tools"`

	// assistant・user
	IsAPIErrorMessage bool `json:"is_api_error_message"`
	Message           struct {
		ID      string  `json:"id"`
		Content []block `json:"content"`
	} `json:"message"`

	// result
	IsError      bool                  `json:"is_error"`
	StopReason   string                `json:"stop_reason"`
	TotalCostUSD float64               `json:"total_cost_usd"`
	Usage        usage                 `json:"usage"`
	ModelUsage   map[string]modelUsage `json:"modelUsage"`
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
	case f.Type == "system" && f.Subtype == "init":
		if s.sawInit {
			return unknown()
		}
		s.sawInit = true
		e, err := ev(v0.TypeSessionStarted, true, map[string]any{
			"agent":         Name,
			"agent_session": f.SessionID,
			"cwd":           f.Cwd,
			"model":         f.Model,
			"tools":         f.Tools,
		})
		return []v0.Envelope{e}, err

	case f.Type == "assistant" && !f.IsAPIErrorMessage: // API の失敗は PR③
		var out []v0.Envelope
		for _, b := range f.Message.Content {
			var e v0.Envelope
			var err error
			switch b.Type {
			case "text":
				e, err = ev(v0.TypeMessageText, true, map[string]any{"message_id": f.Message.ID, "text": b.Text})
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

	case f.Type == "user":
		var out []v0.Envelope
		for _, b := range f.Message.Content {
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

	case f.Type == "result" && !f.IsError: // 失敗のターンは PR③
		var window, maxOut int64
		for _, m := range f.ModelUsage { // モデルが複数なら、大きい方 (窓の上限を、小さく見せない)
			window, maxOut = max(window, m.ContextWindow), max(maxOut, m.MaxOutputTokens)
		}
		u, err := ev(v0.TypeUsage, true, map[string]any{
			"scope":                       "turn",
			"input_tokens":                f.Usage.InputTokens,
			"output_tokens":               f.Usage.OutputTokens,
			"cache_read_input_tokens":     f.Usage.CacheReadInputTokens,
			"cache_creation_input_tokens": f.Usage.CacheCreationInputTokens,
			"cost_usd":                    f.TotalCostUSD,
			"context_window":              window,
			"max_output_tokens":           maxOut,
		})
		if err != nil {
			return nil, err
		}
		t, err := ev(v0.TypeTurnCompleted, true, map[string]any{"stop_reason": stopReason(f.StopReason), "is_error": false})
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

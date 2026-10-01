package opencode

import (
	"bytes"
	"encoding/json"
)

// lenient は、想定外の型の値を、エラーにせず零値にする (agent/claude と同じ。共通化は後の掃除)。frame を 1 回で読むので、1 つの
// フィールドの型の不一致で Unmarshal 全体が失敗すると、フレームが raw ごと失われる。値があったが読めなかったことは Bad に残す。
// 零値が反対の意味を持つもの (承認に使う ID・action など) は、Bad を見て安全側 (承認できない側) に倒す。
type lenient[T any] struct {
	V   T
	Bad bool
}

func (l *lenient[T]) UnmarshalJSON(b []byte) error {
	var v T
	l.V, l.Bad = v, false // 重複したキーは、最後の値だけを見る
	if bytes.Equal(b, []byte("null")) || json.Unmarshal(b, &v) != nil {
		l.Bad = true
		return nil
	}
	l.V = v
	return nil
}

// event は、SSE の data: の JSON 1 行のうち、変換が読む部分 (ほかは宣言しない)。種類は type が持つ (event: 行は無い。ADR 0022)。
type event struct {
	Type lenient[string] `json:"type"`
	Data json.RawMessage `json:"data"`
}

// kind は、SSE の type の扱い。eventKinds が、全ての type の扱いの正で、DecodeFrame・classify_test がこれを使う。
type kind int

const (
	kHousekeeping      kind = iota + 1 // 出さない (空の列)。状態も持たない
	kFrame                             // 語彙が無い (agent.frame)
	kSessionCreated                    // root は session.started・子は追跡だけ
	kToolInputStarted                  // tool 名を覚える (出さない)
	kTextEnded                         // message.text
	kToolCalled                        // tool.call
	kToolProgress                      // tool.update (in_progress)・子 session と call の対応
	kToolSuccess                       // tool.update (completed)
	kToolFailed                        // tool.update (failed)
	kStepEnded                         // usage (scope=step)
	kPermissionAsked                   // permission.requested
	kPermissionReplied                 // permission.resolved (自分が返したものは出さない)
	kFormCreated                       // form.requested
	kFormReplied                       // form.resolved
	kFormCancelled                     // form.resolved (cancelled)
	kExecSucceeded                     // turn.completed
	kExecFailed                        // error + turn.completed
	kExecInterrupted                   // turn.completed (cancelled)
)

// eventKinds は、採取した opencode 2.0.20 の SSE の type (spec/testdata/golden/opencode-serve の 43 種) の扱い。ここに無い type は、
// agent.frame (lenient)。classify_test が、golden の全 type がここにあること・ここの全 type が golden にあることを確かめる。
var eventKinds = map[string]kind{
	"server.connected": kHousekeeping,
	"project.updated":  kHousekeeping, "integration.updated": kHousekeeping, "provider.updated": kHousekeeping,
	"model.updated": kHousekeeping, "agent.updated": kHousekeeping, "command.updated": kHousekeeping, "skill.updated": kHousekeeping,
	"websearch.updated": kHousekeeping, "reference.updated": kHousekeeping, "plugin.updated": kHousekeeping,
	"session.inbox.enqueued": kHousekeeping, "session.inbox.delivered": kHousekeeping, "session.execution.started": kHousekeeping,
	"session.instructions.updated": kHousekeeping, "session.renamed": kHousekeeping, "session.step.started": kHousekeeping,
	"session.step.streamed": kHousekeeping, "session.step.failed": kHousekeeping, "session.tool.input.ended": kHousekeeping,
	"session.text.started": kHousekeeping, "session.text.delta": kHousekeeping, "session.usage.updated": kHousekeeping,
	"shell.created": kHousekeeping, "shell.exited": kHousekeeping,

	"session.retry.scheduled": kFrame, "session.synthetic": kFrame,

	"session.created":               kSessionCreated,
	"session.tool.input.started":    kToolInputStarted,
	"session.text.ended":            kTextEnded,
	"session.tool.called":           kToolCalled,
	"session.tool.progress":         kToolProgress,
	"session.tool.success":          kToolSuccess,
	"session.tool.failed":           kToolFailed,
	"session.step.ended":            kStepEnded,
	"permission.asked":              kPermissionAsked,
	"permission.replied":            kPermissionReplied,
	"form.created":                  kFormCreated,
	"form.replied":                  kFormReplied,
	"form.cancelled":                kFormCancelled,
	"session.execution.succeeded":   kExecSucceeded,
	"session.execution.failed":      kExecFailed,
	"session.execution.interrupted": kExecInterrupted,
}

// validID は、URL の path に入れる ID (session・要求・form) として安全か: [A-Za-z0-9_-] の 1〜128 文字。opencode が振る ID は
// ses_…・per_…・frm_… の形。外れる ID は、承認・応答に使えない (path の区切り・.. ・制御文字を、通さない)。
func validID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// isObject は、m が JSON のオブジェクトか。
func isObject(m json.RawMessage) bool {
	m = bytes.TrimSpace(m)
	return len(m) > 0 && m[0] == '{'
}

// marshal は、HTML 用のエスケープをしない json.Marshal。
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// orNull は、無かった (空の) 値を JSON の null にする (空の RawMessage は、Marshal が失敗する)。
func orNull(m json.RawMessage) json.RawMessage {
	if len(m) == 0 {
		return json.RawMessage("null")
	}
	return m
}

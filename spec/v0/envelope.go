package v0

import "encoding/json"

// Version は、封筒の v の値。この package が定める仕様の版。
const Version = 0

// Envelope は、goronation がセッションごとに振る、イベントの封筒。
// Session は goronation のセッション ID で、エージェント自身の ID (claude の session_id・opencode の
// sessionID) は、TypeSessionStarted の data に載せる。TS は RFC 3339 (UTC・ミリ秒) の、goronation が受け取った時刻。
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	TS      string          `json:"ts"`
	Session string          `json:"session"`
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	Durable bool            `json:"durable"`
	Data    json.RawMessage `json:"data"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// Command は、goronation からエージェントへの指示の封筒。イベントと違い、seq と durable は無い。
type Command struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	TS      string          `json:"ts"`
	Session string          `json:"session"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
}

// イベントの type。各定数の doc に、claude (stream-json) と opencode (run --format json) の、どのフレームから作るかを書く。
// 「合成」は、フレームが無く、アダプタが作ることを指す。
const (
	// TypeSessionStarted は、セッションの開始 (durable)。data は agent・agent_session・cwd など。claude は system/init
	// (ただし 1 回の起動で、ターンごとに繰り返し出る。2 つ目以降は TypeAgentFrame にする)。opencode は init が無いので、
	// 最初のフレームの sessionID から合成する。
	TypeSessionStarted = "session.started"
	// TypeTurnStarted は、ターンの開始 (durable)。どちらのエージェントもフレームを出さないので、prompt の送信で合成する。
	TypeTurnStarted = "turn.started"
	// TypeMessageDelta は、メッセージの途中経過 (durable でない)。予約: 採取では、どちらも部分メッセージを出さなかった。
	TypeMessageDelta = "message.delta"
	// TypeMessageText は、確定したテキスト (durable)。claude は assistant フレームの content の text (1 ブロック 1 フレームで、
	// message.id を共有する)。opencode は type=text のフレーム (part.time.end があり、全文が入る)。
	TypeMessageText = "message.text"
	// TypeToolCall は、tool の呼び出し (durable)。data は call_id・name・kind (ToolKind)・input・status。claude は
	// tool_use の block (id が call_id)。opencode は type=tool_use のフレーム (part.callID)。opencode は完了か失敗の後
	// でしか出さない (実行中の状態のフレームが無い) ので、status=pending のこのイベントを、TypeToolUpdate と組で合成する。
	TypeToolCall = "tool.call"
	// TypeToolUpdate は、tool の状態の更新 (durable)。data は call_id・status・output か error。claude は tool_result の
	// block を含む、type=user のフレーム (ユーザーの発言ではない。tool_use_id が call_id)。opencode は同じ tool フレームの
	// state.status (completed・error)。
	TypeToolUpdate = "tool.update"
	// TypePermissionRequested は、権限の要求 (durable)。予約: 非対話の実行 (claude -p・opencode run) は、要求のフレームを
	// 出さず、拒否の結果だけが出る。対話での要求は、control protocol (claude)・serve (opencode) の側にあり、未採取。
	TypePermissionRequested = "permission.requested"
	// TypePermissionResolved は、権限の決着 (durable)。data は by (policy か human)・outcome (PermissionOptionKind)・rule。
	// 非対話の拒否は、by=policy として合成する。claude は system/permission_denied と、tool_result の is_error と
	// tool_result_meta の non_execution_kind=user-rejected と、result の permission_denials。opencode は tool の
	// state.error (拒否の文)。
	TypePermissionResolved = "permission.resolved"
	// TypeUsage は、使用量 (durable)。data は scope (turn か step)・トークン数・費用・context_window (拡張)。claude は
	// result の usage・modelUsage (contextWindow・maxOutputTokens を含む。ターンごと)。opencode は step_finish の
	// tokens・cost (ステップごと。窓の上限は出ない)。
	TypeUsage = "usage"
	// TypeTurnCompleted は、ターンの終わり (durable)。data は stop_reason (StopReason)・is_error。claude は result
	// (ターンごとに 1 つ。subtype は、失敗でも success になるので見ない)。opencode は、フレームに明示の終わりが無い:
	// step_finish の reason=stop か、それが無いまま (tool-calls で終わる権限拒否) 標準出力が閉じたことから合成する。
	TypeTurnCompleted = "turn.completed"
	// TypeError は、プロバイダーなどの失敗 (durable)。data は status・retryable・message。claude は is_api_error_message が
	// true の assistant フレームと、is_error が true の result (api_error_status・terminal_reason)。opencode は
	// type=error のフレーム (error.data の statusCode・isRetryable・message。ステップのフレームは出ない)。どちらも終了コードは 1。
	// url・応答の本文・ヘッダは、data に載せず raw にだけ残す。
	TypeError = "error"
	// TypeAgentFrame は、対応する語彙が無いフレーム (durable でない)。data は空で、raw に元のフレームを載せる。
	TypeAgentFrame = "agent.frame"
)

// コマンドの type。
const (
	// CommandPrompt は、ユーザーのメッセージを渡す。claude は標準入力への stream-json の 1 行
	// ({"type":"user","message":{"role":"user","content":[{"type":"text","text":...}]}})。opencode の run は 1 メッセージで
	// 終わるので、ターンごとに --continue で起動し直す。
	CommandPrompt = "prompt"
	// CommandCancel は、ターンの中断。予約 (未採取)。
	CommandCancel = "cancel"
	// CommandPermissionResolve は、権限の要求への応答。data は request_id と outcome (PermissionOptionKind)。予約 (未採取)。
	CommandPermissionResolve = "permission.resolve"
)

// StopReason は、TypeTurnCompleted の stop_reason。ACP の StopReason (end_turn・max_tokens・max_turn_requests・
// refusal・cancelled) に、失敗のターン用の StopError を拡張として足す (ACP では、失敗は JSON-RPC のエラーになる)。
// 採取で現れたのは、end_turn (claude の result.stop_reason・opencode の reason=stop) と、失敗の 2 つ。
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
	StopError           = "error"
)

// ToolStatus は、tool の状態で、ACP の ToolCallStatus と同じ (pending・in_progress・completed・failed)。
const (
	ToolPending    = "pending"
	ToolInProgress = "in_progress"
	ToolCompleted  = "completed"
	ToolFailed     = "failed"
)

// PermissionOptionKind は、権限の応答の種類で、ACP の PermissionOptionKind と同じ。
const (
	AllowOnce    = "allow_once"
	AllowAlways  = "allow_always"
	RejectOnce   = "reject_once"
	RejectAlways = "reject_always"
)

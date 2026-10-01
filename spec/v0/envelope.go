package v0

import "encoding/json"

// Version は、封筒の v の値。この package が定める仕様の版。
const Version = 0

// Envelope は、goronation がセッションごとに振る、イベントの封筒。イベントストア・監査ログ向けの内部の表現。
// Raw を含むので、UI・API にそのまま返さない。返すときは Public() を使う (限界は Public の doc)。
// Session は goronation のセッション ID で、エージェント自身の ID (claude の session_id・opencode の
// sessionID) は、TypeSessionStarted の data に載せる。TS は RFC 3339 (UTC・ミリ秒) の、goronation が受け取った時刻。
type Envelope struct {
	V       int    `json:"v"`
	ID      string `json:"id"`
	TS      string `json:"ts"`
	Session string `json:"session"`
	Seq     uint64 `json:"seq"`
	Type    string `json:"type"`
	Durable bool   `json:"durable"`
	// Origin は、サブエージェントの出力のとき、その帰属 (ADR 0045)。メインのエージェント自身の出力は nil。
	Origin *Origin         `json:"origin,omitempty"`
	Data   json.RawMessage `json:"data"`
	Raw    json.RawMessage `json:"raw,omitempty"`
}

// Origin は、サブエージェントの出力の帰属。サブエージェントの発言・tool 呼び出し・権限要求が、メインのエージェント自身のものとして
// 表示されないようにする (見えないものを、承認させない)。claude は、parent_tool_use_id が null でないフレーム。opencode は、子の session
// (session.created の parentID) のイベント。
type Origin struct {
	// ID は、サブエージェントの識別子 (claude: parent_tool_use_id か task の ID。opencode: 子の session の ID)。
	ID string `json:"id"`
	// Parent は、サブエージェントを起動した tool 呼び出しの call_id (分かれば)。
	Parent string `json:"parent,omitempty"`
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
	// TypeSessionStarted は、セッションの開始 (durable)。data は agent・agent_session・cwd・permission_mode (claude。default 以外は、tool が承認なしで実行されうる) など。claude は system/init
	// (ただし 1 回の起動で、ターンごとに繰り返し出る。2 つ目以降は TypeAgentFrame にする)。opencode は init が無いので、
	// 最初のフレームの sessionID から合成する。
	TypeSessionStarted = "session.started"
	// TypeTurnStarted は、ターンの開始 (durable)。どちらのエージェントもフレームを出さないので、prompt の送信で合成する。
	// data は text (送った prompt の全文。画面に「送った文」を出すため。ADR 0010)。
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
	// TypePermissionRequested は、権限の要求 (durable)。data は request_id (応答に使う、エージェントが振った ID)・call_id (対応する
	// tool 呼び出し)・tool_name・kind (ToolKind)・input (書き換えない)・title (人間向けの短い説明。無ければ空)。claude は、
	// --permission-prompt-tool stdio で起動したときの、can_use_tool の control_request (ADR 0010)。それ以外の subtype と、応答できない形
	// (request_id・tool_name が無い、input がオブジェクトでない) の要求は TypeAgentFrame にする。同じ request_id の再要求は、先の要求を
	// 上書きせず TypeAgentFrame にする。permission_suggestions は載せない。非対話の実行 (claude -p・opencode run) は、要求のフレームを出さず、
	// 拒否の結果だけが出る。opencode の対話での要求は、serve の permission.asked (data の id が request_id・source.id が call_id・action が
	// tool_name・input は {action, resources, metadata})。ADR 0041 の追加の欄: summary (機械生成の 1 行)・details (ラベルつきの項目)・
	// details_truncated・content_hash (ADR 0042。共通の層が付ける)。型は PermissionRequested。
	TypePermissionRequested = "permission.requested"
	// TypePermissionResolved は、権限の決着 (durable)。data は by・outcome (PermissionOptionKind か、cancelled)。
	//   - by=policy: 非対話の拒否 (data は by・outcome=reject_once・call_id・tool_name)。claude は system/permission_denied で、
	//     tool_result の is_error と tool_result_meta の non_execution_kind=user-rejected と、result の permission_denials からは出さない。
	//     opencode は tool の state.error (拒否の文)。
	//   - by=human: 対話の応答 (data は by・outcome・request_id)。フレームは無く、CommandPermissionResolve の送信で合成する。
	//   - by=agent: エージェントが未決の要求を取り下げた (data は by・outcome=cancelled・request_id)。outcome の cancelled は、
	//     PermissionOptionKind ではない拡張 (ACP の RequestPermissionOutcome の cancelled と同じ意味)。claude は control_cancel_request か、
	//     未決のまま result (ターンの終わり) が来たとき。
	// 要求 (request_id) には、決着がちょうど 1 つ付く。
	TypePermissionResolved = "permission.resolved"
	// TypeFormRequested は、エージェントが人間にフィールドの一覧を入力させる要求 (durable。ADR 0040)。data は FormRequested。claude は
	// AskUserQuestion の can_use_tool (requires_user_interaction:true。permission.requested にはしない)。opencode は form.created
	// (question の tool・web 検索の provider の選択など。metadata.kind が kind)。応答は CommandFormResolve。
	TypeFormRequested = "form.requested"
	// TypeFormResolved は、form の決着 (durable)。data は request_id・by (human・agent・policy)・outcome (FormAnswered・FormCancelled)・
	// answer (answered のとき)。要求 (request_id) には、決着がちょうど 1 つ付く。
	TypeFormResolved = "form.resolved"
	// TypeUsage は、使用量 (durable)。data は scope (turn か step)・トークン数・費用・context_window (拡張)。claude は
	// result の usage・modelUsage (contextWindow・maxOutputTokens を含む。ターンごと)。opencode は step_finish の
	// tokens・cost (ステップごと。窓の上限は出ない)。
	TypeUsage = "usage"
	// TypeTurnCompleted は、ターンの終わり (durable)。data は stop_reason (StopReason)・is_error。claude は result
	// (ターンごとに 1 つ。subtype は、失敗でも success になるので見ない)。opencode は、フレームに明示の終わりが無い:
	// step_finish の reason=stop か、それが無いまま (tool-calls で終わる権限拒否) 標準出力が閉じたことから合成する。
	TypeTurnCompleted = "turn.completed"
	// TypeError は、プロバイダーなどの失敗 (durable)。data は status・retryable・message。claude は is_error が true の
	// result (api_error_status・terminal_reason)。同じ内容の is_api_error_message の assistant フレームは、agent.frame にする。opencode は
	// type=error のフレーム (error.data の statusCode・isRetryable・message。ステップのフレームは出ない)。どちらも終了コードは 1。
	// url・ヘッダは、data に載せず raw にだけ残す。message は claude が整えた文で、応答の本文・request_id を含みうる。
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
	// CommandPermissionResolve は、権限の要求への応答。data は request_id と outcome (M1.5 で受けるのは allow_once と reject_once だけ)。
	// claude は control_response の 1 行 (許可する input は、要求時に保持した値だけから作る。reject の message は固定文)。
	// 未決でない request_id (未知・応答済み・撤回済み) への応答は error で、何も書かない。合成する TypePermissionResolved (by=human) を返す。
	CommandPermissionResolve = "permission.resolve"
	// CommandFormResolve は、form への応答 (ADR 0040)。data は FormResolve (request_id・outcome・answer・content_hash)。回答は、保持した要求のフィールド
	// に対して検査し (FormResolve.Validate)、通らなければ error で、何も書かない。claude は control_response の allow で、保持した input に answers
	// (質問の文 → 回答の文字列。複数選択は ", " で連結) を足した updatedInput を返す (ADR 0044)。取り消しは deny。opencode は POST …/form/{id}/reply と DELETE。
	CommandFormResolve = "form.resolve"
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

// ToolKind は、TypeToolCall の data の kind で、ACP の ToolKind と同じ。tool の名前は、エージェントごとに違うので、
// 利用者 (UI) が、名前を知らなくても、種類で見分けられるようにするもの。対応表は、アダプタの側にある。
const (
	KindRead       = "read"
	KindEdit       = "edit"
	KindDelete     = "delete"
	KindMove       = "move"
	KindSearch     = "search"
	KindExecute    = "execute"
	KindThink      = "think"
	KindFetch      = "fetch"
	KindSwitchMode = "switch_mode"
	KindOther      = "other"
)

// PermissionOptionKind は、権限の応答の種類で、ACP の PermissionOptionKind と同じ。
const (
	AllowOnce    = "allow_once"
	AllowAlways  = "allow_always"
	RejectOnce   = "reject_once"
	RejectAlways = "reject_always"
)

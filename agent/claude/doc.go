// Package claude は、Claude Code (claude -p --output-format stream-json) のフレームと、標準形式 (spec/v0) の封筒を、
// 互いに変換する、core/agent.Adapter の実装。
//
// プロセスの起動にも、檻にも関わらない。1 行ずつのバイト列を変換するだけ (起動は cmd の役目)。import してよいのは cmd/** だけ
// (tools/archtest の impl-only-from-cmd)。根拠のフレームは spec/testdata/golden/claude。
//
// # 使い方
//
// Adapter{}.NewStream() を 1 回の起動ごとに作り、標準出力の 1 行ごとに DecodeFrame、標準入力への書き込みに EncodeCommand を使う。
//
// # 規則
//
//   - init: system/init の 1 回目は TypeSessionStarted、2 回目以降は TypeAgentFrame。prompt は TypeTurnStarted を合成。
//   - message: assistant の text は TypeMessageText、tool_use は TypeToolCall。user の tool_result は TypeToolUpdate (is_error なら failed)。
//     result は TypeUsage・TypeTurnCompleted (subtype は失敗でも success なので見ない)。
//   - permission-request: can_use_tool の control_request は TypePermissionRequested。permission.resolve (allow_once・reject_once) は
//     control_response と、合成の TypePermissionResolved (by=human)。許可する input は要求時に保持した値だけ。未決でない ID は error。
//     control_cancel_request と、未決のままの result は by=agent・outcome=cancelled。同じ ID の再要求は TypeAgentFrame (上書きしない)。
//     最初の prompt の raw は、先頭に initialize の control_request を含む (ADR 0010)。
//   - permission-denied: system/permission_denied は TypePermissionResolved (by=policy・reject_once。message は path を含むので載せない)。
//   - error: is_error が true の result は TypeError を先頭に足す (stop_reason=error)。is_api_error_message の assistant は TypeAgentFrame。
//   - unknown-kept: 上に無いフレームは TypeAgentFrame (data は空、raw に元のフレーム)。
//   - data-allowlist: data は UI・API に出る。語彙が必要とする値だけを名前を付けて写し、frame を丸ごとは写さない (書き換えずに載せるのは
//     tool の input・tool_result の content・テキスト)。conformance_test.go が、golden の全フレームで確かめる。
//
// # 限界
//
//   - 対話の権限要求は、--permission-prompt-tool stdio (隠しフラグ) 付きの起動でだけ出る。採取した版は TestedVersion で、版が上がると
//     形が変わりうるので、未知は TypeAgentFrame に倒す。承認フローの完全性は、標準出力が pipe の間は保証できない (Stream の doc・ADR 0010)。
//   - 未採取 (部分メッセージ・推論・ターンの中断・再開・MCP・サブエージェント) は TypeAgentFrame。Grep・Glob・MultiEdit の名前は、組み込みの名前から決めた。
package claude

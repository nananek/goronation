// Package claude は、Claude Code (claude -p --output-format stream-json) のフレームと、標準形式 (spec/v0) の封筒を、
// 互いに変換する、core/agent.Adapter の実装。
//
// プロセスの起動にも、檻にも関わらない。渡された 1 行ずつのバイト列を、変換するだけ (起動は cmd の役目)。
// この package を import してよいのは cmd/** だけ (tools/archtest の impl-only-from-cmd)。根拠のフレームは spec/testdata/golden/claude。
//
// # 使い方
//
//	s := claude.Adapter{}.NewStream()      // 1 回の起動ごとに 1 つ
//	envs, err := s.DecodeFrame(line)       // 標準出力の 1 行 (改行なし)
//	raw, synth, err := s.EncodeCommand(c)  // prompt を、標準入力に書く 1 行と、合成するイベントにする
//
// # 規則
//
//   - init: system/init の 1 回目は TypeSessionStarted、2 回目以降 (ターンごとに繰り返し出る) は TypeAgentFrame。
//   - message: assistant の content の text は TypeMessageText、tool_use は TypeToolCall (ブロックごとに 1 イベント)。
//   - tool-result: user の content の tool_result は TypeToolUpdate (人間の発言ではない。is_error が true なら failed)。
//   - result: result は TypeUsage と TypeTurnCompleted (この順)。subtype は、失敗でも success になるので見ない。
//   - prompt: prompt (Command) は標準入力の 1 行。フレームの無い TypeTurnStarted は、このときに合成する。
//   - permission-denied: system/permission_denied は TypePermissionResolved (by=policy・outcome=reject_once・call_id・tool_name)。
//     message は、実物では文字列で (assistant・user ではオブジェクト)、path を含むので data に載せない。同じ事実を指す tool_result の
//     is_error・result の permission_denials からは、二重に出さない (tool_result は tool.update の failed になる)。
//     claude は、拒否があったターンも result の is_error=false で終わるので、turn.completed の is_error は false のまま。
//     拒否は、permission.resolved と、failed の tool.update で分かる。
//   - unknown-kept: 上に無いフレームは、捨てずに TypeAgentFrame にする (data は空、raw に元のフレーム)。
//   - data-allowlist: data は UI・API に出る (Envelope.Public は Raw だけを落とす)。封筒の語彙が必要とする値だけを、名前を付けて写し、
//     frame を丸ごと・部分木ごと写さない。書き換えずに載せる約束の値は、tool の input・tool_result の content・テキスト。
//     写していない値が data に出ないことは、conformance_test.go が、golden fixtures の全フレームで確かめる。
//
// # 限界
//
//   - API の失敗 (is_api_error_message が true の assistant フレーム・is_error が true の result) は未対応 (PR③)。
//     TypeAgentFrame になり、失敗のターンの turn.completed は出ない。
//   - message がオブジェクトでない assistant・user のフレームは、error にせず TypeAgentFrame にする (error にすると、フレームが raw ごと失われる)。
//   - 未採取のフレーム (部分メッセージ・推論・対話での権限要求・中断・再開・MCP・サブエージェント) は、TypeAgentFrame になる。
//   - Grep・Glob・MultiEdit の tool の名前は、採取した system/init の tools に無く、claude の組み込みの名前から決めた。
package claude

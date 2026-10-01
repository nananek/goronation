// Package v0 は、goronation の標準形式 (spec/v0) の初版: エージェント (claude・opencode) のフレームを写す、封筒とイベント・コマンドの語彙を定める。
//
// 封筒の形と、語彙ごとの、claude・opencode のフレームとの対応 (各定数の doc) を保証する。対応は、
// spec/testdata/golden の実フレーム (M0 スパイク S1) を根拠にし、採れなかったもの (下の限界) は保証しない。
// アダプタ (フレームから封筒への変換) と、イベントストアは、この package の外 (M1) にある。
//
// # 方針
//
//   - seq と id と ts は、goronation が振る。エージェントのフレームには、通し番号も、信頼できる時刻も無い。
//   - seq は、durable でないイベントにも振る。再送 (after=seq) は durable だけを返すので、欠番は異常ではない。
//   - イベント (エージェントから goronation) とコマンド (goronation からエージェント) は、別の型に分ける。
//   - 語彙は ACP から借りる (StopReason・ToolStatus・PermissionOptionKind・ToolKind)。無いものは拡張で、doc に書く。
//   - エージェント固有の入力・出力の値 (tool の input など) は、書き換えず data に載せる。
//   - raw は、元のフレーム。path・設定・応答の本文を含みうるので、UI へは出さない。
//
// # 限界
//
//   - 採れていないもの: 部分メッセージ (delta)・推論・claude のサブエージェント (Agent tool)・再開・MCP。TypeMessageDelta と
//     CommandCancel は、形だけを予約する。claude の対話での権限要求と応答は 2.1.284 で、AskUserQuestion は 2.1.286 で採った (ADR 0010・0044)。
//   - opencode は、run --format json (1 メッセージずつ。spec/testdata/golden/opencode) と、serve の HTTP + SSE (opencode 2.0.20。
//     spec/testdata/golden/opencode-serve。権限・form・subagent・中断を含む) で採った。語彙の対応は、後者を正とする (ADR 0040・0041)。
//   - 値 (モデル名・トークン数・費用) は fake の値で、意味が無い。構造だけを見る。
//
// # 関連
//
// ADR 0008 (決定と理由)・ADR 0010 (対話の権限要求)・ADR 0040〜0045 (form・要約と詳細・内容ハッシュ・サブエージェントの帰属。form.go・permission.go・hash.go・conformance_test.go)。根拠のフレームは spec/testdata/golden、対応を固定するテストは golden_test.go。
package v0

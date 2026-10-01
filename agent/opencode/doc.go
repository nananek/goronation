// Package opencode は、opencode 2.x (serve の HTTP + SSE) の SSE の 1 行と、標準形式 (spec/v0) の封筒を、互いに変換する、
// core/agent.Adapter の実装。
//
// HTTP の実行にも、SSE の読み取りにも、檻にも関わらない。入力は SSE の data: の JSON 1 行で、書く側は HTTP 要求の記述を返すだけ
// (実行は transport。ADR 0022)。import してよいのは cmd/** だけ (tools/archtest の impl-only-from-cmd)。根拠のフレームは
// spec/testdata/golden/opencode-serve。
//
// # 使い方
//
// Adapter{}.NewStream() を 1 回の起動ごとに作り、SSE の 1 行ごとに DecodeFrame、書き込みに EncodeCommand を使う。
//
// # 規則
//
//   - session: 最初の parentID の無い session.created が root (session.started)。子 (parentID) は追跡だけで、root でも子でもない
//     session のイベントは agent.frame。子のイベントは Origin つき (ADR 0045)。
//   - mapping: ADR 0049 の表のとおり (eventKinds)。housekeeping・delta・usage.updated は出さない。usage は step.ended (scope=step)。
//   - request: permission.asked は permission.requested (要約・詳細。ADR 0041)、form.created は form.requested (ADR 0040)。形が不正・
//     上限 (未決 64) 超えは承認できない側 (TypeError)。同じ ID の再要求は agent.frame。詳細が切れた要求は、許可できない。
//   - settle: 要求の決着はちょうど 1 つ。自分が返したものは EncodeCommand が合成し、SSE の replied は出さない。root・子の終わりで、
//     未決は by=agent・cancelled。turn.completed は root の execution.* だけ。
//   - command: ADR 0050。allow_always・reject_always は error。form の回答は、保持した form に対して検査する。ID は validID だけ。
//   - data-allowlist: data は語彙が必要とする値だけ (leak_test.go が golden で確かめる)。error に url・ヘッダ・response.body は載せない。
//
// # 限界
//
//   - 採取は 2.0.20 (TestedVersion)。SSE の出どころが opencode 自身であることに頼る (ADR 0010・0022 と同じ)。未採取の type は agent.frame。
//   - delta・再試行の通知・推論は出さない (UI が描けるまで。ADR 0049)。root の決定は、SSE の最初の session.created に依る (transport が
//     session の作成を行う PR⑥ で、その session ID と一致することを確かめる)。
package opencode

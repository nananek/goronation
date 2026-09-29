// Command framecapture は、M0 スパイク S1 (Issue #1) のためのフレーム採取ハーネス。
//
// bwrap 檻の中で実物の claude・opencode を動かし、tools/fakeproviders の fake Anthropic Messages API・fake
// OpenAI 互換 Chat Completions API に向けて、クレジットを使わずに実際のフレームを採取する。この結果を
// 踏まえて spec/v0 (標準形式の封筒) を ADR 化するための、調査専用のツール。cmd/goronation とは別バイナリ
// (bwrap を起動する os/exec は cmd/** にしか置けない)。Linux 専用 (bwrap 依存)。
//
// # 使い方
//
//	framecapture claude -- "こんにちは"
//	framecapture opencode --scenario spec/testdata/golden/scenarios/tool-call.json --normalize
//
// # 方針
//
// fake サーバーへは、internal/connectproxy (テスト専用・SSRF 対策なしの、最小限の CONNECT プロキシ) で
// 到達させる。本物の資格情報・ネットワークには使わない (egress.Server とは別物。詳細は connectproxy)。
//
// 場面 (--scenario) は、ターンと、fake の応答 (エージェント別) と、置くファイルの JSON。--normalize は、
// 実行ごとに変わる値 (ID・時刻・所要時間) を固定の記号にして、再実行で同じフレーム列にする。
// spec/testdata/golden/capture.sh が、場面をすべて採り直す (--check は、コミット済みの fixtures との一致だけを見る)。
//
// # 限界
//
//   - push・MCP・PTY・セッションの永続化・認証情報の共有は無い (goronation run とは別物)。
//   - opencode の複数ターンは、起動を分けて --continue でつなぐ。opencode が最初のターンに別に投げる
//     タイトル生成のリクエストには、場面の応答を使わず、固定の題を返す。
//   - --normalize は、JSON のキーの順序を保たない (キー名の辞書順になる)。
package main

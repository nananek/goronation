// Package main (cmd/framecapture) は、M0 スパイク S1 (Issue #1) のためのフレーム採取ハーネス。
//
// bwrap 檻の中で実物の claude・opencode を動かし、tools/fakeproviders の fake Anthropic Messages
// API・fake OpenAI 互換 Chat Completions API に向けて、クレジットを使わずに実際のフレームを採取する。
// この結果を踏まえて spec/v0 (標準形式の封筒) を ADR 化する (PR④) ための、調査専用のツール。
// cmd/goronation とは別バイナリ (I1・設計ルール1: bwrap を起動する os/exec は cmd/** にしか置けない。
// cmd/goronation 本体は変更しない)。Linux 専用 (bwrap 依存)。
//
// fake サーバーへの到達には、cmd/framecapture/connectproxy (このハーネス専用の、最小限の CONNECT
// プロキシ) を使う。egress.Server は使わない: egress.Server は、loopback・private な宛先への dial を
// SSRF 対策として無条件に拒む設計で、fake サーバーをホストの loopback に置くこのハーネスの用途とは
// 相容れないため (詳細は doc key s1-pr2-design)。connectproxy は、egress.Server のような汎用の
// ポリシーエンジンではなく、起動時に渡した小さな固定の対応表 (仮想ホスト名 → fake サーバーの実アドレス)
// だけを許可する。本物の資格情報・本物のネットワークに触れる経路には、絶対に使わない。
//
// # 使い方
//
//	framecapture opencode -- "こんにちは"
//	framecapture claude   -- "こんにちは"
//
// # 限界 (M0 スパイクとしての割り切り)
//
//   - push・MCP・PTY・セッションの永続化・認証情報の共有は無い (goronation run とは別物)。
//   - 1 回の起動で 1 ターンだけ採取する (opencode の --session による複数ターン採取は PR③)。
//   - fake サーバーの応答は、単純な固定文 (--response) だけ (ツール呼び出し・エラー等のシナリオ
//     スクリプトは PR③)。
package main

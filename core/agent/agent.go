// Package agent は、エージェント (claude・opencode) のフレームと、標準形式 (spec/v0) の封筒を、互いに変換する port を置く。
//
// 実装 (agent/claude など) は import しない。この規則は go.mod では守れないため、tools/archtest が強制する。
// 実装は、プロセスの起動にも、檻にも関わらない。渡された 1 行ずつのバイト列を、変換するだけ。
package agent

import v0 "github.com/nananek/goronation/spec/v0"

// Adapter は、1 種類のエージェント (claude・opencode) の実装。名前をキーにした表 (cmd が持つ) で選ぶ。
type Adapter interface {
	// Name は、エージェントの名前 ("claude"・"opencode")。
	Name() string
	// NewStream は、1 回のプロセス起動に紐づく、状態を持つ変換器を作る。起動ごとに作り、使い回さない。
	NewStream() Stream
}

// Stream は、実行中の 1 つのエージェントのプロセスに紐づく、状態を持つ変換器。
// 同じ起動の中でだけ意味のある状態 (init を見たか・実行中の tool など) を持つので、複数の起動で共有しない。
// 並行には呼べない (1 つの起動の出力は 1 行ずつ順に来る)。
//
// 返す Envelope の ID・TS・Session・Seq は、空のまま返す。goronation (呼び手) が振る (ADR 0008)。
// V・Type・Durable・Data・Raw は、Stream が決める。
type Stream interface {
	// DecodeFrame は、エージェントの出力の 1 行 (改行を含まない) を、0 個以上の Envelope に変換する。
	// 対応する語彙が無いフレームは、捨てずに v0.TypeAgentFrame にする。JSON でない行など、解釈できないものは error を返す。
	DecodeFrame(raw []byte) ([]v0.Envelope, error)
	// EncodeCommand は、Command を、エージェントの入力に書くバイト列にする。
	// フレームが無く、コマンドの送信で合成するイベント (v0.TypeTurnStarted など) は、synthesized で返す。
	// 未対応のコマンドは error を返す。
	EncodeCommand(cmd v0.Command) (raw []byte, synthesized []v0.Envelope, err error)
}

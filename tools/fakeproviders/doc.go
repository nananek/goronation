// Package fakeproviders は、M0 スパイク S1 (フレーム採取) 用の、外部 AI provider の最小限の fake サーバー群を置く。
//
// anthropic (Anthropic Messages API) と openai (OpenAI 互換 Chat Completions API) の 2 つの
// サブパッケージからなる。クレジットを使わずに、本物の claude / opencode を実際に動かして、
// 標準形式 v0 (spec/) の元になる実物のフレームを採取するために使う。bwrap・egress には依存しない。
package fakeproviders

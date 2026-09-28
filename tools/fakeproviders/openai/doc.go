// Package openai は、OpenCode の @ai-sdk/openai-compatible provider が話す、OpenAI 互換の
// Chat Completions API の、最小限の fake サーバー。
//
// POST /v1/chat/completions を実装し、あらかじめ決めた応答 (Step) をスクリプトして、本物とほぼ同じ形の
// SSE ストリーミング応答、または非ストリーミング応答を返す。M0 スパイク S1 (フレーム採取) 専用で、
// 認証・レート制限・トークン数の正確さは実装しない。
//
// # 使い方
//
//	srv := httptest.NewServer(openai.NewServer(
//		openai.Step{Content: "こんにちは"},
//	))
//	defer srv.Close()
//	// opencode.json の provider.options.baseURL に srv.URL を渡して、本物の opencode を向ける。
//
// # 方針
//
//   - リクエストボディの "stream" フィールドに従い、true なら SSE の chunk 列 (末尾は "data: [DONE]")、
//     false なら単一の JSON を返す。
//   - Authorization ヘッダ・リクエストボディの妥当性は検査しない (fake なので何でも受理する)。
//   - Step が尽きたら、最後の Step を繰り返す (何回でも呼べる)。
package openai

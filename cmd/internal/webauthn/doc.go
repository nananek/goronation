// Package webauthn は、goro serve の Web ログイン (WebAuthn によるセッション認証) を実装する。
//
// 対応するのは、1 ユーザー・1 passkey・ES256 (P-256 ECDSA) だけの最小限の骨組み: 登録 (シェルで発行した
// 1 回限りのブートストラップトークンで、最初の 1 回だけ) → ログイン → 署名済みのセッション token を発行する。
// Vault (PRF) は範囲外 (goro-serve-plan §4-2 (b)。後回し)。
//
// # 使い方
//
//	st, err := webauthn.NewStore(stateDir)
//	cfg := webauthn.Config{RPID: "example.com", Origin: "https://example.com"}
//	opts, state, err := webauthn.RegisterBegin(ctx, cfg, st, token) // ブラウザへ返す
//	err = webauthn.RegisterFinish(ctx, cfg, st, state, resp)        // 検証して保存
//	session, err := webauthn.AuthenticateFinish(ctx, cfg, st, state, resp) // ログイン成功時
//
// # 規則
//
//   - stateless: challenge はサーバー側に保存せず、HMAC 署名した token (state) に載せてブラウザへ返し、
//     検証の要求にそのまま付けて返させる (token.go)。credential は常に高々 1 つ、すでにあれば登録は断る
//     (作り直すには状態ファイルを消す。goro serve reset のようなコマンドは無い)。
//   - none-attestation: 登録は attestation "none" だけを要求・受理する (信頼チェーンの検証はしない)。
//     対応する COSE のアルゴリズムは ES256 だけ。拡張データ (authenticatorData の ED フラグ) は拒否する。
//   - state-file: 状態は、credfile.Store 経由で <state>/credentials/webauthn に置く (0600・pinned・
//     atomic write は credfile に委ねる)。中身は JSON で、[]byte のフィールドは base64 になる。
//   - session-epoch: セッション token は発行時点の世代番号 (SessionEpoch) を埋め込む。Logout は世代を
//     進め、それ以前の token (盗まれたものも含む) を一括で失効させる (credential・state token は変えない)。
//
// # 限界
//
//   - 1 ユーザー・1 credential 固定。RP ID を変えると既存の credential は使えなくなるが、登録し直す手段は無い。
//   - signCount のクローン検知・レート制限・total lockout は、この package にも呼び手にも無い。
package webauthn

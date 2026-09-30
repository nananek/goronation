// Package webauthn は、goronation serve の Web ログインと、Vault の操作 (解錠・passkey の追加・削除) の認証を、WebAuthn で実装する。
//
// ADR 0033・0038・0039 に従う。1 ユーザー・ES256 (P-256 ECDSA)・複数の passkey (最大 16)。最初の 1 つは、シェルで発行した 1 回限りのブートストラップトークン。
// 2 つ目以降は、既存の passkey の認可で足す。
//
// # 使い方
//
//	opts, state, err := webauthn.RegisterBegin(ctx, cfg, st, token)        // 最初の 1 つ → RegisterFinish
//	opts, state, err := webauthn.AuthenticateBegin(ctx, cfg, st)            // ログイン → AuthenticateFinish
//	opts, state, err := webauthn.OpAuthBegin(ctx, cfg, st, binding, evals)  // 操作つきの再認証 (UV・PRF) → OpAuthFinish
//	opts, state, err := webauthn.AddBegin(ctx, cfg, st, requestID)          // passkey の追加 → AddFinish → CommitAdd
//	err = webauthn.RemoveCredential(ctx, st, target, authz)                 // passkey の削除
//
// # 規則
//
//   - stateless: challenge は、HMAC 署名した state token に載せて往復する (token.go)。操作つきの state は、操作の種類・対象の
//     credential ID・要求の ID・応えてよい passkey も署名の中に持ち、Finish は、呼び手の値との一致を見る。応答は 1 回しか通らない。
//   - none-attestation: attestation "none" だけ。ED フラグの拡張データは、許可リスト (hmac-secret・credProtect) だけ受け、
//     未知のキー・型違い・余りは拒否する (authdata.go)。
//   - prf-unverified: PRF の出力は clientExtensionResults (署名の外) にあり、検証できない。呼び手は、復号を試す入力にだけ使い、
//     Wipe で消す。追加・削除の認可は、署名の検証 (UV 必須) と、呼び手の vault の証明で行う。
//   - state-file: credfile.NewSized 経由で <state>/credentials/webauthn (16 KiB まで)。単数だった版の形は、読むときに移す。
//
// # 限界
//
//   - 状態の「読んで保存」の直列化 (Store.mu) はプロセスの中だけ。別プロセスの CLI (IssueBootstrapToken) が、web の書き込みと重なると、
//     どちらかの更新が失われうる (ファイルロックは未実装)。
//   - signCount のクローン検知・レート制限・total lockout は無い。S9 (iPhone 実機) で、PRF・ED の実際の挙動を確かめる。
//   - 追加した passkey の PRF が、作成時に返らない場合の再認証は、未実装。SAS (ADR 0035) の配線も、この package の外。
package webauthn

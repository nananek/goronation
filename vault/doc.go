// Package vault は、passkey の PRF 出力から作るラップ鍵で本体鍵を包み、本体鍵で、資格情報とログイン状態を暗号化して、1 つのディレクトリ (0700) に保管する Vault の実装である。
//
// 保証するのは、鍵の階層 (ADR 0033)・ラップの追加と削除の条件 (ADR 0038)・reset (ADR 0034)。保証しない: WebAuthn の検証・SAS・
// 解錠の要求の認可 (呼び手の責任。ADR 0037 では web)。PRF 出力は、検証できない値で、復号を試す入力にだけ使う。
// 標準ライブラリと core だけを使い、プロセスを起動しない。import してよいのは cmd/** だけ (ADR 0036)。
//
// # 使い方
//
//	v, err := vault.Open(dir)                            // 施錠中で開く。dir は 0700・flock で占有する
//	err = v.Init(credentialID, salt, prf)                // 初回: 最初のラップ。以後 Unlock(credentialID, prf)
//	_, err = vault.Reset(dir, vault.ResetOptions{})      // 壊すだけ。dir の flock を取れなければ ErrInUse
//
// # 規則
//
//   - add-only: ラップの追加は AddWrap だけで、同じ credential ID があれば ErrWrapExists (上書きしない)。
//   - remove-once: ラップの削除は RemoveWrap だけ。解錠済み・別の passkey の証明・最後の 1 つは消せない。
//   - proof: 認可は、authorizer の PRF が、自分のラップを復号できること。解錠中かは見ない。candidate の PRF は認可に使わない。
//   - no-change-on-fail: 偽の PRF 値・未登録の credential ID で、保存内容は変わらない (認可の失敗は ErrUnlockFailed。長さ・形の不正は ErrInvalidInput)。
//   - fresh-key: 項目は、書き込みごとの乱数で、鍵を変える。AAD は Vault の ID・種類・名前・形式の版。
//   - audit-first: init・追加・削除・reset は、監査の記録 (追記のみ・0600) を先に書く。書けなければ、進めない。秘密は書かない。
//
// # 限界
//
//   - 監査の記録は、同じ uid の改ざん (切り詰め) を検知しない。ディスクの写しと、漏れた PRF 出力があれば、復号できる。
//   - Go のメモリから、鍵を確実には消せない (clear は、この package が持つ slice だけ)。mlock・dumpable=0 は、デーモン (ADR 0037) の側。
package vault

// Package vault は、Vault の port (署名・ログイン状態の保管と復元) と、施錠中の error の契約を定める。
//
// 実装は top module の vault/ にあり、この package を import する側から見えない。資格情報の取り出しは、port を足さず、
// core/credential の Source を使う。解錠・passkey の追加と削除・reset は port にしない (呼べるのは cmd/** だけ)。
// LoginStore の中身は、檻の中のエージェントが書いた認証ファイルで、敵対入力として、上限を検査する。
// 保証しない: 内容の正しさ・メモリからの消去・Signer の実装 (gpg を動かす檻の形は、M5 で決める)。
//
// # 規則
//
//   - locked: 施錠中の操作は ErrLocked (errors.Is で判定する)。呼び手は失敗として扱い、値の無い状態で続けない。
//   - state-limit: 1 状態あたり 8 ファイル・1 ファイル 64 KiB・合計 256 KiB。名前は相対 (filepath.IsLocal)・128 バイト以内・重複なし。
//   - state-name: 名前は Clean 済みの UTF-8 で、制御文字 (C0・DEL・C1) を含まない。
//   - check-both: Put でも Get (復元) でも、CheckFiles を通す。
//
// # 関連
//
// docs/adr/0032-vault-core-ports.md
package vault

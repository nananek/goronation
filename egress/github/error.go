package github

import (
	"errors"
	"fmt"
	"strconv"
)

// 拒否の理由の識別子 (Error.Code)。監査の reason にそのまま出せる: 固定の短い文字列で、要求の中身を含まない。
const (
	CodeBadRoute       = "bad-route"        // 経路 (method・path・query) が、POST .../pulls ではない
	CodeTooLarge       = "too-large"        // 要求・応答が、大きさの上限を超える
	CodeBadJSON        = "bad-json"         // JSON として正しくない (構文・UTF-8・末尾の続き・値の型)
	CodeUnknownField   = "unknown-field"    // 許可していない項目 (大文字違いのキーを含む)
	CodeDuplicateField = "duplicate-field"  // 同じ項目が 2 回ある
	CodeBadField       = "bad-field"        // 項目の値が、規則に合わない (長さ・文字・型)
	CodeRepoNotAllowed = "repo-not-allowed" // PR を作ってよい repo ではない
	CodeHeadNotAllowed = "head-not-allowed" // head が、許可したブランチ (セッション専用の名前空間) ではない
	CodeQuota          = "quota"            // セッションの PR の作成数の上限に達した
	CodeBadResponse    = "bad-response"     // 上流の応答が、期待した形ではない
)

// Error は、要求・応答を断ったことを表す。Code は監査に出す識別子 (Code* の定数) で、Msg は人が読む説明。
// Error の文字列は、要求に含まれていた文字を、引用して切り詰めた形でしか含まない: 端末に出しても、制御文字・エスケープシーケンスは通らない。
type Error struct {
	Code string
	Msg  string
}

// Error は、Code と Msg を 1 行にした文字列を返す。
func (e *Error) Error() string { return "github: " + e.Code + ": " + e.Msg }

// Reason は、err が要求・応答を断ったこと (*Error) なら、その Code を返す。それ以外は空。
func Reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// reject は、Error を作る。format に渡す、要求由来の文字列は、q を通す。
func reject(code, format string, a ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// maxQuoted は、q が残す長さ (バイト)。
const maxQuoted = 48

// q は、要求由来の文字列を、メッセージに入れてよい形にする: 長ければ切り、ASCII の引用符つき (制御文字・非 ASCII は \x や \u で表す)。
func q(s string) string {
	if len(s) > maxQuoted {
		return strconv.QuoteToASCII(s[:maxQuoted]) + "..."
	}
	return strconv.QuoteToASCII(s)
}

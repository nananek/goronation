package git

import (
	"errors"
	"fmt"
	"strconv"
)

// 拒否の理由の識別子 (Error.Code)。監査の reason にそのまま出せる: 固定の短い文字列で、要求の中身を含まない。
const (
	CodeBadRoute       = "bad-route"         // 経路 (method・path・query) が、許す 4 つのどれでもない
	CodeBadPacket      = "bad-packet"        // pkt-line の形が正しくない (長さの書き方・0001 などの特別な長さ・空の行)
	CodeTruncated      = "truncated"         // 本文が、コマンド部の途中で終わった
	CodeTooLarge       = "too-large"         // コマンド部が、大きさの上限を超える
	CodeTooManyCmds    = "too-many-commands" // コマンドが、件数の上限を超える
	CodeBadCommand     = "bad-command"       // "old new ref" の形が正しくない (oid・ref の文字・NUL の位置)
	CodeBadCapability  = "bad-capability"    // 許可していない capability・重複・書き方の誤り
	CodePushOptions    = "push-options"      // push-options は受けない
	CodeShallow        = "shallow"           // shallow の行は受けない
	CodePushCert       = "push-cert"         // push-cert は受けない
	CodeTrailingData   = "trailing-data"     // コマンドが無いのに、続きがある
	CodeDelete         = "delete"            // ref の削除は受けない
	CodeRefNotAllowed  = "ref-not-allowed"   // 許可した名前空間の外の ref
	CodeRepoNotAllowed = "repo-not-allowed"  // 許可した repo ではない
)

// Error は、要求を断ったことを表す。Code は監査に出す識別子 (Code* の定数) で、Msg は人が読む説明。
// Error の文字列は、要求に含まれていた文字を、引用して切り詰めた形でしか含まない: 端末に出しても、制御文字・エスケープシーケンスは通らない。
type Error struct {
	Code string
	Msg  string
}

// Error は、Code と Msg を 1 行にした文字列を返す。
func (e *Error) Error() string { return "git: " + e.Code + ": " + e.Msg }

// Reason は、err が要求を断ったこと (*Error) なら、その Code を返す。それ以外 (読み取りの失敗など) は空。
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

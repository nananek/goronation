package chat

import v0 "github.com/nananek/goronation/spec/v0"

// 応答の API (POST /form・POST /permission) の本文の型は、spec/v0 の型の別名で、cmd/goronation は spec/v0 を import できない (tools/archtest の
// v0-only-in-chat) ので、この package を通して使う。JSON の読み方 (回答は文字列か文字列の配列だけ) は、v0.FormValue が持つ。
type (
	// FormResolve は、form への応答 (v0.FormResolve の別名)。
	FormResolve = v0.FormResolve
	// PermissionResolve は、権限の要求への応答 (v0.PermissionResolve の別名)。
	PermissionResolve = v0.PermissionResolve
)

const (
	// MaxFormFields は、form のフィールドの数の上限 (spec/v0)。本文の上限の計算に使う。
	MaxFormFields = v0.MaxFormFields
	// MaxFormAnswerText は、回答 1 つの文字数 (rune) の上限 (spec/v0)。本文の上限の計算に使う。
	MaxFormAnswerText = v0.MaxFormAnswerText
)

package main

import (
	"bytes"
	"testing"
)

// 攻撃レビュー: 値を引数に書いてしまったとき (NAME の位置・NAME の後ろ・オプション名の位置) の、使い方の誤りの表示に、値が出てはならない。
// PR 本文は「error・表示に値を含めない (名前・使い方を含む)」「goro auth の全ての失敗経路で、stdout・stderr に値が出ない」、
// goro auth -h は「値は、画面・履歴・ログ・エラーに出さない」と宣言している。引数の値はシェルの履歴・ps に既に残るが、
// 端末の画面・scrollback・録画・CI のログには、さらに写してはならない。
func TestAttackArgvTokenIsNotEchoed(t *testing.T) {
	for name, args := range map[string][]string{
		"NAME の後ろに値 (goro auth github <値>)":   {"github", authTok},
		"NAME の位置に値 (goro auth <値>)":          {authTok},
		"オプション名の位置に値 (goro auth github -<値>)": {"github", "-" + authTok},
		"--state-dir の後ろの余りに値":                {"github", "--state-dir", t.TempDir(), authTok},
	} {
		var out, errb bytes.Buffer
		code := runAuth(args, pipeStdin(t, ""), &out, &errb)
		if code == 0 {
			t.Errorf("%s: 終了コード 0 (使い方の誤りとして断るはず)", name)
		}
		if p := leaksAuth(out.String() + errb.String()); p != "" {
			t.Errorf("%s: 出力に値 (%q…) が出た:\nstdout: %q\nstderr: %q", name, p, out.String(), errb.String())
		}
	}
}

package opencode

import (
	"strings"
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// patch の項目の label ("patch: <file>") は、MaxDetailLabelLen (64) で切られる。切れた file の後ろは、どの項目にも出ない
// (resources と違う file のとき) ので、許可できない。見せていない path を承認させない (ADR 0041 決定 3)。
func TestLongPatchFileNameCannotBeAllowed(t *testing.T) {
	long := strings.Repeat("d/", 40) + "etc/cron.d/evil" // "patch: " を足すと 64 文字を超える
	s := started(t)
	feed(t, s, "permission.asked", asked("per_1", "ses_root", "edit", `["notes.txt"]`, `"metadata":{"files":[{"file":"`+long+`","patch":"x"}]}`))
	if _, _, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err == nil {
		t.Fatalf("label で切れた file (%d 文字) を持つ要求を、許可できた", len(long))
	}
	if _, _, err := cmdPermission(t, s, "per_1", v0.RejectOnce); err != nil {
		t.Fatalf("拒否はできる: %v", err)
	}
}

// 対照: label が 64 文字に収まる file は、許可できる。
func TestShortPatchFileNameCanBeAllowed(t *testing.T) {
	s := started(t)
	feed(t, s, "permission.asked", asked("per_1", "ses_root", "edit", `["notes.txt"]`, `"metadata":{"files":[{"file":"`+strings.Repeat("a", 57)+`","patch":"x"}]}`))
	if _, _, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err != nil {
		t.Fatal(err)
	}
}

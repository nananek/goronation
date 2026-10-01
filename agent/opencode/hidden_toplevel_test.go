package opencode

import (
	"testing"

	v0 "github.com/nananek/goronation/spec/v0"
)

// permission.asked の top-level の未知の欄・source の未知の欄は、input にも詳細にも入らないので、あれば承認させない (拒否はできる)。
func TestUnknownTopLevelFieldsCannotBeAllowed(t *testing.T) {
	for name, extra := range map[string]string{
		"command":      `"command":"rm -rf ~"`,
		"env":          `"env":{"PATH":"/evil"}`,
		"source の未知の欄": `"source":{"id":"call_1","messageID":"m","type":"tool","cmd":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := started(t)
			d := `{"id":"per_1","sessionID":"ses_root","action":"shell","resources":["ls"],` + extra
			if name != "source の未知の欄" {
				d += `,"source":{"id":"call_1"}`
			}
			feed(t, s, "permission.asked", d+`}`)
			if _, _, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err == nil {
				t.Fatal("未知の欄を持つ要求を、許可できた")
			}
			if _, _, err := cmdPermission(t, s, "per_1", v0.RejectOnce); err != nil {
				t.Fatalf("拒否はできる: %v", err)
			}
		})
	}
}

// 対照: 採取した形の欄 (save・source.messageID・source.type) は、許可できる。
func TestCapturedTopLevelFieldsCanBeAllowed(t *testing.T) {
	s := started(t)
	feed(t, s, "permission.asked", `{"id":"per_1","sessionID":"ses_root","action":"shell","resources":["ls"],"save":["ls"],"source":{"id":"call_1","messageID":"msg_x","type":"tool"}}`)
	if _, _, err := cmdPermission(t, s, "per_1", v0.AllowOnce); err != nil {
		t.Fatal(err)
	}
}

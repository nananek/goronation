package chat

import (
	"strings"
	"testing"
)

func TestParseHTTPRequestAllowed(t *testing.T) {
	for _, c := range []struct{ name, line, method, path string }{
		{"prompt", `{"method":"POST","path":"/api/session/ses_1/prompt","body":{"text":"hi"}}`, "POST", "/api/session/ses_1/prompt"},
		{"interrupt", `{"method":"POST","path":"/api/session/ses_1/interrupt","body":null}`, "POST", "/api/session/ses_1/interrupt"},
		{"permission", `{"method":"POST","path":"/api/session/ses_1/permission/per_2/reply","body":{"decision":"once"}}`, "POST", "/api/session/ses_1/permission/per_2/reply"},
		{"form reply", `{"method":"POST","path":"/api/session/ses_c/form/frm_1/reply","body":{"answer":{"q0":"Red"}}}`, "POST", "/api/session/ses_c/form/frm_1/reply"},
		{"form cancel", `{"method":"DELETE","path":"/api/session/ses_1/form/frm_1","body":null}` + "\n", "DELETE", "/api/session/ses_1/form/frm_1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := ParseHTTPRequest([]byte(c.line))
			if err != nil {
				t.Fatal(err)
			}
			if r.Method != c.method || r.Path != c.path {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestParseHTTPRequestRejected(t *testing.T) {
	for name, line := range map[string]string{
		"GET":                   `{"method":"GET","path":"/api/session/ses_1/prompt","body":{"text":"x"}}`,
		"PUT":                   `{"method":"PUT","path":"/api/session/ses_1/prompt","body":{"text":"x"}}`,
		"他の path":               `{"method":"POST","path":"/api/other","body":{}}`,
		"session の外":            `{"method":"POST","path":"/api/session","body":{}}`,
		"..":                    `{"method":"POST","path":"/api/session/../x/prompt","body":{}}`,
		"//":                    `{"method":"POST","path":"/api/session//prompt","body":{}}`,
		"%":                     `{"method":"POST","path":"/api/session/ses%2f1/prompt","body":{}}`,
		"制御文字":                  `{"method":"POST","path":"/api/session/ses_1\n/prompt","body":{}}`,
		"NUL":                   `{"method":"POST","path":"/api/session/ses_1\u0000/prompt","body":{}}`,
		"query":                 `{"method":"POST","path":"/api/session/ses_1/prompt?x=1","body":{"text":"x"}}`,
		"未知の語":                  `{"method":"POST","path":"/api/session/ses_1/shell","body":{}}`,
		"形が違う method":           `{"method":"DELETE","path":"/api/session/ses_1/permission/per_1/reply","body":null}`,
		"form の POST (reply 無)": `{"method":"POST","path":"/api/session/ses_1/form/frm_1","body":{}}`,
		"未知の欄":                  `{"method":"POST","path":"/api/session/ses_1/interrupt","body":null,"x":1}`,
		"body が配列":              `{"method":"POST","path":"/api/session/ses_1/prompt","body":[1]}`,
		"body が文字列":             `{"method":"POST","path":"/api/session/ses_1/prompt","body":"x"}`,
		"prompt に body 無":       `{"method":"POST","path":"/api/session/ses_1/prompt","body":null}`,
		"interrupt に body":      `{"method":"POST","path":"/api/session/ses_1/interrupt","body":{"a":1}}`,
		"DELETE に body":         `{"method":"DELETE","path":"/api/session/ses_1/form/frm_1","body":{}}`,
		"後ろに余計":                 `{"method":"POST","path":"/api/session/ses_1/interrupt","body":null} {"x":1}`,
		"JSON でない":              `POST /api/session/ses_1/prompt`,
		"空":                     ``,
		"長い id":                 `{"method":"POST","path":"/api/session/` + strings.Repeat("a", 129) + `/interrupt","body":null}`,
		"大きすぎる":                 `{"method":"POST","path":"/api/session/ses_1/prompt","body":{"text":"` + strings.Repeat("a", MaxHTTPRequest) + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if r, err := ParseHTTPRequest([]byte(line)); err == nil {
				t.Fatalf("通った: %+v", r)
			}
		})
	}
}

func FuzzParseHTTPRequest(f *testing.F) {
	f.Add([]byte(`{"method":"POST","path":"/api/session/ses_1/prompt","body":{"text":"hi"}}`))
	f.Add([]byte(`{"method":"DELETE","path":"/api/session/s/form/f","body":null}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		r, err := ParseHTTPRequest(line)
		if err != nil {
			return
		}
		if !strings.HasPrefix(r.Path, "/api/session/") || strings.ContainsAny(r.Path, "%?#.\\ \x00\r\n") || strings.Contains(r.Path, "//") {
			t.Fatalf("危ない path が通った: %q", r.Path)
		}
		if r.Method != "POST" && r.Method != "DELETE" {
			t.Fatalf("method = %q", r.Method)
		}
	})
}

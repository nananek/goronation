package main

import (
	"strings"
	"testing"
)

// ADR 0028 の 3: 「経路のヘッダ (Forwarded・X-Forwarded-* など) は、大文字小文字・重複・空白に関わらず捨てる」。
// X-Forwarded- で始まるヘッダは、名前を問わず上流に届けない。
func TestParseRequestDropsAllXForwarded(t *testing.T) {
	for _, name := range []string{"X-Forwarded-Server", "X-Forwarded-Scheme", "X-Forwarded-Uri", "X-Forwarded-Path", "X-Forwarded-Ssl", "x-forwarded-by", "X-FORWARDED-FOR", "X-Forwarded-"} {
		t.Run(name, func(t *testing.T) {
			req, err := parse(t, "GET /api/info HTTP/1.1\r\n"+name+": evil\r\n\r\n")
			if err != nil {
				t.Fatalf("拒否された: %v", err)
			}
			if strings.Contains(strings.ToLower(string(req.head)), "x-forwarded") {
				t.Errorf("%s が上流に届いた:\n%s", name, req.head)
			}
		})
	}
	// 接頭辞が違うだけの、ふつうのヘッダは落とさない。
	req, err := parse(t, "GET /api/info HTTP/1.1\r\nX-Forward: a\r\nX-Opencode-Directory: /work\r\n\r\n")
	if err != nil || !strings.Contains(string(req.head), "X-Forward: a") || !strings.Contains(string(req.head), "X-Opencode-Directory: /work") {
		t.Errorf("ふつうの X- ヘッダが落ちた: %v\n%s", err, req.head)
	}
}

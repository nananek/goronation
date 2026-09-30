package main

import (
	"errors"
	"testing"
)

// ADR 0030: 経路のヘッダ (X-Forwarded-* で始まる名前すべて) は、許可リストに無いので、名前を問わず 400 で拒否する (上流に届けない)。
func TestParseRequestRejectsAllXForwarded(t *testing.T) {
	for _, name := range []string{"X-Forwarded-Server", "X-Forwarded-Scheme", "X-Forwarded-Uri", "X-Forwarded-Path", "X-Forwarded-Ssl", "x-forwarded-by", "X-FORWARDED-FOR", "X-Forwarded-"} {
		t.Run(name, func(t *testing.T) {
			req, err := parse(t, "GET /api/info HTTP/1.1\r\n"+name+": evil\r\n\r\n")
			var re *relayError
			if !errors.As(err, &re) || re.status != 400 {
				t.Errorf("%s が拒否されない: req=%v err=%v", name, req != nil, err)
			}
		})
	}
}

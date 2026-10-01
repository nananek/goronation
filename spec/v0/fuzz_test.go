package v0

import (
	"encoding/json"
	"testing"
)

// FuzzFormResolve は、クライアントが送る回答 (JSON) を、読んで、保持した要求に対して検査する: どんな入力でも panic せず、
// 検査を通った回答は、(1) 要求のフィールドのキーだけを持ち、(2) JSON に戻して読み直しても、同じ結果になる。
func FuzzFormResolve(f *testing.F) {
	for _, s := range []string{
		`{"request_id":"r1","outcome":"answered","answer":{"q0":"Red","q1":["S"],"q2":"x"}}`,
		`{"request_id":"r1","outcome":"cancelled"}`,
		`{"request_id":"r1","outcome":"answered","answer":{"q0":["Red"]}}`,
		`{"request_id":"r1","outcome":"answered","answer":{"q0":null}}`,
		`{"request_id":"r1","outcome":"answered","answer":{"q0":"Red","q1":["S",null]}}`,
		`{"request_id":"r1","outcome":"answered","answer":{"q0":"Red","q1":[1]}}`,
		`{"request_id":"r1","outcome":"answered","answer":{"q0":"Red","q1":[""]}}`,
		`{"request_id":"r1","outcome":"answered","answer":{"zz":"x"},"content_hash":"sha256:00"}`,
		`{"request_id":"r1","outcome":"answered","answer":{"q0":"Red","q0":"Blue"}}`,
		`[]`, `null`, `{"answer":1}`,
	} {
		f.Add([]byte(s))
	}
	req := sampleForm()
	f.Fuzz(func(t *testing.T, b []byte) {
		var r FormResolve
		if json.Unmarshal(b, &r) != nil {
			return
		}
		if r.Validate(req) != nil {
			return
		}
		fields := map[string]bool{}
		for _, fd := range req.Fields {
			fields[fd.Key] = true
		}
		for k := range r.Answer {
			if !fields[k] {
				t.Fatalf("検査を通った回答に、要求に無いキー %q がある", k)
			}
		}
		for k, v := range r.Answer { // 配列の中の null などが、空文字列に化けて、検査を通っていない
			for _, x := range v.Values {
				if v.Multi && x == "" {
					t.Fatalf("multiselect %q に、空文字列が通った", k)
				}
			}
		}
		again, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("検査を通った回答を、JSON にできない: %v", err)
		}
		var r2 FormResolve
		if err := json.Unmarshal(again, &r2); err != nil || r2.Validate(req) != nil {
			t.Fatalf("読み直すと、通らない: %v %s", err, again)
		}
	})
}

// FuzzPermissionHash は、入力 (JSON) から作った要求の Hash が、(1) panic せず、(2) 同じ要求で、いつも同じ値になることを確かめる。
func FuzzPermissionHash(f *testing.F) {
	f.Add([]byte(`{"a":1,"b":[true,null,"x"]}`), "Write", "summary")
	f.Add([]byte(`{"n":1.0e3,"s":"<&>"}`), "Bash", "")
	f.Fuzz(func(t *testing.T, input []byte, tool, summary string) {
		p := PermissionRequested{RequestID: "r", ToolName: tool, Kind: KindOther, Input: json.RawMessage(input), Summary: summary}
		h1, h2 := p.Hash(), p.Hash()
		if h1 != h2 {
			t.Fatal("同じ要求で、ハッシュが違う")
		}
		if p.Validate() == nil && h1 == "" {
			t.Fatal("検査を通った要求のハッシュが、空 (計算できない)")
		}
	})
}

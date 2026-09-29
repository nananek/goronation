//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestNormalizeFramesIDsAreNumberedByFirstAppearance(t *testing.T) {
	in := `{"session_id":"11111111-1111-1111-1111-111111111111","uuid":"22222222-2222-2222-2222-222222222222"}
{"session_id":"11111111-1111-1111-1111-111111111111","uuid":"33333333-3333-3333-3333-333333333333"}
`
	got, err := normalizeFrames([]byte(in))
	if err != nil {
		t.Fatalf("normalizeFrames: %v", err)
	}
	want := `{"session_id":"<uuid:1>","uuid":"<uuid:2>"}
{"session_id":"<uuid:1>","uuid":"<uuid:3>"}
`
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNormalizeFramesSameShapeDifferentRunsAreEqual(t *testing.T) {
	// 別の実行 (ID・時刻・所要時間だけが違う) は、正規化すると同じになる。
	a := `{"type":"text","timestamp":1790644836079,"sessionID":"ses_aaa","part":{"id":"prt_x1","messageID":"msg_y1","time":{"start":1790644836049,"end":1790644836063}}}` + "\n" +
		`{"type":"result","duration_ms":123,"total_cost_usd":0.0005,"ttft_ms":7,"session_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","timestamp":"2026-09-29T01:18:29.671Z"}` + "\n"
	b := `{"type":"text","timestamp":1790644999999,"sessionID":"ses_bbb","part":{"id":"prt_x2","messageID":"msg_y2","time":{"start":1,"end":2}}}` + "\n" +
		`{"type":"result","duration_ms":999,"total_cost_usd":0.0009,"ttft_ms":1,"session_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","timestamp":"2026-09-30T05:00:00.000Z"}` + "\n"
	na, err := normalizeFrames([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	nb, err := normalizeFrames([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	if string(na) != string(nb) {
		t.Fatalf("正規化しても違う:\n%s\n%s", na, nb)
	}
	for _, leaked := range []string{"1790644836079", "ses_aaa", "0.0005", "2026-09-29", "aaaaaaaa"} {
		if strings.Contains(string(na), leaked) {
			t.Errorf("実行ごとの値 %q が残っている:\n%s", leaked, na)
		}
	}
}

func TestNormalizeFramesKeepsStableValues(t *testing.T) {
	in := `{"type":"assistant","text":"こんにちは <b>&</b>","n":12,"nested":{"id":"toolu_fake_1","input_tokens":10},"list":[1,"a",null,true]}` + "\n"
	got, err := normalizeFrames([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	// キーは辞書順になる。値 (日本語・HTML の記号・数値・null・固定の ID) はそのまま。
	want := `{"list":[1,"a",null,true],"n":12,"nested":{"id":"toolu_fake_1","input_tokens":10},"text":"こんにちは <b>&</b>","type":"assistant"}` + "\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNormalizeFramesIsIdempotent(t *testing.T) {
	in := `{"a":"ses_abc","b":{"time":{"start":5}},"duration_ms":9}` + "\n"
	once, err := normalizeFrames([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	twice, err := normalizeFrames(once)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatalf("2 回目で変わった:\n%s\n%s", once, twice)
	}
}

func TestNormalizeFramesIDNumberingDoesNotDependOnMapOrder(t *testing.T) {
	// キー名の順に辿るので、何度やっても同じ番号になる (map の走査順に依らない)。
	in := `{"z":"11111111-1111-1111-1111-111111111111","a":"22222222-2222-2222-2222-222222222222","m":"33333333-3333-3333-3333-333333333333"}` + "\n"
	want := `{"a":"<uuid:1>","m":"<uuid:2>","z":"<uuid:3>"}` + "\n"
	for i := 0; i < 50; i++ {
		got, err := normalizeFrames([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("i=%d got %s", i, got)
		}
	}
}

func TestNormalizeFramesRejectsNonJSON(t *testing.T) {
	if _, err := normalizeFrames([]byte("{\"a\":1}\nnot json\n")); err == nil {
		t.Fatal("JSON でない行があるのに、error にならない")
	}
}

func TestNormalizeFramesSkipsBlankLines(t *testing.T) {
	got, err := normalizeFrames([]byte("\n{\"a\":1}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"a\":1}\n" {
		t.Fatalf("got %q", got)
	}
}

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nananek/goronation/cmd/internal/framenorm"
)

// opencode 2.x の serve の golden fixtures (spec/testdata/golden/opencode-serve。採取は capture_opencode_serve_linux_test.go) の、CI で走る検査。
// fixtures は、PR⑤ (アダプタ) の適合テストの入力になるので、場面の抜け・正規化の漏れ・秘密の混入を、ここで止める。

const serveGoldenDir = "../../spec/testdata/golden/opencode-serve"

// serveSceneNames は、採取する場面の名前 (serveScenes と、fixtures のディレクトリが、これに一致する)。
var serveSceneNames = []string{
	"simple-text", "tool-call-shell-allow", "tool-call-shell-reject", "tool-call-shell-always", "pending-snapshot", "actions", "subagent", "multi-turn",
	"question-single", "question-rule-allow", "question-multiple", "question-custom", "question-cancel", "websearch-form",
	"interrupt-streaming", "interrupt-pending-permission",
	"error-provider-500", "error-provider-400", "error-provider-bad-stream", "error-provider-truncated",
}

var epochRe = regexp.MustCompile(`[^0-9a-zA-Z_.-]1[5-9][0-9]{11}[^0-9]`) // 2017 年以降の epoch ミリ秒 (13 桁)

// fixtureProblems は、fixtures (の 1 ファイル) に、資格情報・ホストの path・時刻の残りがあれば、その説明を返す。
func fixtureProblems(hostDir, name string, b []byte) []string {
	var out []string
	bad := []string{"Authorization", "Basic ", "OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD", "/run/user", "x-api-key", hostDir}
	if h, err := os.UserHomeDir(); err == nil && h != "" && h != "/" {
		bad = append(bad, h)
	}
	if m := epochRe.Find(b); m != nil && !strings.HasSuffix(name, "meta.json") {
		out = append(out, name+" に、時刻 (epoch ミリ秒) らしい数 "+strings.TrimSpace(string(m))+" が残っている (正規化の漏れ)")
	}
	for _, s := range bad {
		if s != "" && bytes.Contains(b, []byte(s)) {
			out = append(out, name+" に、入ってはいけない文字列 "+s+" がある")
		}
	}
	return out
}

// assertCleanFixture は、fixtureProblems を t のエラーにする。
func assertCleanFixture(t *testing.T, hostDir, name string, b []byte) {
	t.Helper()
	for _, p := range fixtureProblems(hostDir, name, b) {
		t.Error(p)
	}
}

func TestFixtureProblemsCatchesLeaks(t *testing.T) {
	for in, want := range map[string]int{
		`{"h":"Authorization: Basic abc"}`:                2,
		`{"t":1790799077149}`:                             1,
		`{"p":"/tmp/goro-host/run/proxy.sock"}`:           1,
		`{"t":0,"id":"<ses:1>","p":"/work","v":"2.0.20"}`: 0,
		`{"n":12345678901234567}` + "\n":                  0, // 13 桁の数の一部ではない (長い ID)
	} {
		if got := fixtureProblems("/tmp/goro-host", "x.ndjson", []byte(in)); len(got) != want {
			t.Errorf("%s: 検出 %d 件 (期待 %d): %v", in, len(got), want, got)
		}
	}
}

type goldenEvent struct {
	Type string `json:"type"`
	Data struct {
		ID, RequestID, Reply, Action string
		ParentID                     string `json:"parentID"`
		Form                         struct {
			Metadata struct{ Kind string }
		}
		Error struct{ Type string }
	} `json:"data"`
}

func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			t.Errorf("%s: 空行がある", path)
			continue
		}
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	return out
}

func TestOpencodeServeGolden(t *testing.T) {
	entries, err := os.ReadDir(serveGoldenDir)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	want := slices.Clone(serveSceneNames)
	slices.Sort(want)
	slices.Sort(dirs)
	if !slices.Equal(dirs, want) {
		t.Fatalf("場面のディレクトリ %v が、期待 %v と違う", dirs, want)
	}
	scene := map[string][]goldenEvent{}
	for _, name := range serveSceneNames {
		dir := filepath.Join(serveGoldenDir, name)
		var meta struct {
			Scene, OpencodeVersion, Description string
		}
		mb, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil || json.Unmarshal(mb, &meta) != nil {
			t.Fatalf("%s/meta.json: %v", name, err)
		}
		if meta.Scene != name || !strings.HasPrefix(meta.OpencodeVersion, "2.0.") || meta.Description == "" {
			t.Errorf("%s/meta.json の内容が不正: %+v", name, meta)
		}
		// 番号の体系は、要求 → イベントの順に 1 つ (採取と同じ順)。正規化済みなら、もう一度かけても変わらない。
		norm := framenorm.New(framenorm.Serve)
		for _, f := range []string{"requests.ndjson", "events.ndjson"} {
			raw, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			assertCleanFixture(t, "", name+"/"+f, raw)
			again, err := norm.Frames(raw)
			if err != nil {
				t.Errorf("%s/%s: %v", name, f, err)
			} else if !bytes.Equal(again, raw) {
				t.Errorf("%s/%s: 正規化済みでない (framenorm.Serve をかけると変わる)", name, f)
			}
		}
		for _, l := range readLines(t, filepath.Join(dir, "events.ndjson")) {
			var e goldenEvent
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatalf("%s/events.ndjson: %v", name, err)
			}
			scene[name] = append(scene[name], e)
		}
		evs := scene[name]
		if len(evs) < 3 || evs[0].Type != "server.connected" {
			t.Errorf("%s: 最初のイベントが server.connected でない", name)
		}
		if last := evs[len(evs)-1].Type; !slices.Contains([]string{"session.execution.succeeded", "session.execution.failed", "session.execution.interrupted"}, last) {
			t.Errorf("%s: 最後のイベントが、execution の終わりでない: %s", name, last)
		}
	}

	// 場面ごとの、観察したはずの事象 (宣言と実態の食い違いの回帰)。
	has := func(name string, pred func(goldenEvent) bool) bool { return slices.ContainsFunc(scene[name], pred) }
	typ := func(s string) func(goldenEvent) bool { return func(e goldenEvent) bool { return e.Type == s } }
	ask := func(a string) func(goldenEvent) bool {
		return func(e goldenEvent) bool { return e.Type == "permission.asked" && e.Data.Action == a }
	}
	for _, a := range []string{"edit", "read", "external_directory", "glob", "grep", "webfetch", "skill"} {
		if !has("actions", ask(a)) {
			t.Errorf("actions: permission.asked の action %q が無い", a)
		}
	}
	for name, p := range map[string]func(goldenEvent) bool{
		"tool-call-shell-allow":  ask("shell"),
		"subagent":               ask("subagent"),
		"question-single":        ask("question"),
		"websearch-form":         ask("websearch"),
		"tool-call-shell-reject": func(e goldenEvent) bool { return e.Type == "permission.replied" && e.Data.Reply == "reject" },
		"tool-call-shell-always": func(e goldenEvent) bool { return e.Type == "permission.replied" && e.Data.Reply == "always" },
	} {
		if !has(name, p) {
			t.Errorf("%s: 期待した permission のイベントが無い", name)
		}
	}
	if !has("subagent", func(e goldenEvent) bool { return e.Type == "session.created" && e.Data.ParentID != "" }) {
		t.Error("subagent: parentID つきの session.created (子の session) が無い")
	}
	// question の allow の規則 (ADR 0043): permission.asked が出ず、form.created が直接出る。
	if has("question-rule-allow", typ("permission.asked")) || !has("question-rule-allow", typ("form.created")) || !has("question-rule-allow", typ("form.replied")) {
		t.Error("question-rule-allow: permission.asked が出ているか、form.created・form.replied が無い")
	}
	for _, name := range []string{"question-single", "question-multiple", "question-custom"} {
		if !has(name, func(e goldenEvent) bool { return e.Type == "form.created" && e.Data.Form.Metadata.Kind == "question" }) || !has(name, typ("form.replied")) {
			t.Errorf("%s: form.created (kind question)・form.replied が無い", name)
		}
	}
	if !has("question-cancel", typ("form.cancelled")) || !has("websearch-form", typ("form.cancelled")) {
		t.Error("form.cancelled が無い")
	}
	if !has("interrupt-streaming", typ("session.execution.interrupted")) || !has("interrupt-pending-permission", typ("session.execution.interrupted")) {
		t.Error("中断の場面に session.execution.interrupted が無い")
	}
	if !has("error-provider-500", typ("session.retry.scheduled")) || !has("error-provider-500", typ("session.execution.failed")) {
		t.Error("error-provider-500: 再試行 (session.retry.scheduled)・失敗 (session.execution.failed) が無い")
	}
	for _, name := range []string{"error-provider-400", "error-provider-bad-stream"} {
		if !has(name, typ("session.execution.failed")) {
			t.Errorf("%s: session.execution.failed が無い", name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(serveGoldenDir, "simple-text", "tools.json")); err != nil || !json.Valid(b) {
		t.Errorf("simple-text/tools.json: %v", err)
	}
}

//go:build linux

package main

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/chat"
)

// 構造化チャットの画面 (assets/chat) のテスト。描画は textContent だけ (エージェントの出力は敵対入力) であることの検査、ページ・静的ファイルの配信、
// node による表示ロジック (chat-core.js・chat.js。偽の DOM・EventSource) の試験。

// 描画に使ってはいけない API (文字列を HTML・コード・URL として解釈するもの)。コメントを除いて検査する。
var chatForbiddenAPIs = map[string]*regexp.Regexp{
	"innerHTML":                regexp.MustCompile(`\binnerHTML\b`),
	"outerHTML":                regexp.MustCompile(`\bouterHTML\b`),
	"insertAdjacentHTML":       regexp.MustCompile(`\binsertAdjacent(HTML|Element|Text)\b`),
	"document.write":           regexp.MustCompile(`\bdocument\s*\.\s*write(ln)?\b`),
	"eval":                     regexp.MustCompile(`\beval\s*\(`),
	"new Function":             regexp.MustCompile(`\bnew\s+Function\b|\bFunction\s*\(`),
	"createContextualFragment": regexp.MustCompile(`createContextualFragment|\bDOMParser\b|\bsrcdoc\b`),
	"setTimeout(string)":       regexp.MustCompile(`\bset(Timeout|Interval)\s*\(\s*['"` + "`" + `]`),
	"on* 属性・ハンドラの代入":           regexp.MustCompile(`\.on[a-z]+\s*=[^=]|setAttribute\s*\(\s*['"]on`),
	"href・src の代入":             regexp.MustCompile(`\.(href|src|action|formAction)\s*=[^=]|setAttribute\s*\(\s*['"](href|src|style|action)`),
	"javascript:":              regexp.MustCompile(`javascript:`),
	"動的 import":                regexp.MustCompile(`\bimport\s*\(`),
	"cookie・storage":           regexp.MustCompile(`\bdocument\s*\.\s*cookie\b|\blocalStorage\b|\bsessionStorage\b`),
}

var (
	jsBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	jsLineComment  = regexp.MustCompile(`(?m)^\s*//.*$|\s//\s.*$`)
)

// forbiddenAPIs は、src (コメントを除く) が使っている、禁止の API の名前。
func forbiddenAPIs(src string) []string {
	src = jsBlockComment.ReplaceAllString(src, "")
	src = jsLineComment.ReplaceAllString(src, "")
	var found []string
	for name, re := range chatForbiddenAPIs {
		if re.MatchString(src) {
			found = append(found, name)
		}
	}
	return found
}

func TestChatUINoDangerousAPIs(t *testing.T) {
	// 検査自体が効いていること (禁止のものを、1 つずつ確かめる)。
	for name, bad := range map[string]string{
		"innerHTML":                "el.innerHTML = x;",
		"outerHTML":                "el.outerHTML = x;",
		"insertAdjacentHTML":       "el.insertAdjacentHTML('beforeend', x);",
		"document.write":           "document.write(x);",
		"eval":                     "eval(x);",
		"new Function":             "new Function(x)();",
		"createContextualFragment": "r.createContextualFragment(x);",
		"setTimeout(string)":       "setTimeout('f()', 1);",
		"on* 属性・ハンドラの代入":           "el.onclick = f;",
		"href・src の代入":             "a.href = x;",
		"javascript:":              "x = 'javascript:alert(1)';",
		"動的 import":                "import('x');",
		"cookie・storage":           "document.cookie;",
	} {
		if got := forbiddenAPIs(bad); !contains(got, name) {
			t.Errorf("%s を含む原稿を、検査が見逃した: %q → %v", name, bad, got)
		}
	}
	if got := forbiddenAPIs("// innerHTML は使わない\n/* eval( */\nx.textContent = y; // a.href = 1\n"); len(got) != 0 {
		t.Errorf("コメントを、禁止の使用と数えた: %v", got)
	}
	for _, name := range []string{"assets/chat/chat.js", "assets/chat/chat-core.js"} {
		b, err := chatAssets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := forbiddenAPIs(string(b)); len(got) != 0 {
			t.Errorf("%s が、禁止の API を使っている: %v", name, got)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ページは、インラインの script・style・イベントハンドラを持たない (CSP が禁じる)。読み込むのは、自前の静的ファイルだけ。
func TestChatUIPageHasNoInlineCode(t *testing.T) {
	b, err := chatAssets.ReadFile("assets/chat/chat.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)<script(\s[^>]*)?>\s*[^<\s]`),      // 中身のある script
		regexp.MustCompile(`(?i)<style`),                           //
		regexp.MustCompile(`(?i)\sstyle\s*=`),                      //
		regexp.MustCompile(`(?i)\son[a-z]+\s*=`),                   //
		regexp.MustCompile(`(?i)javascript:`),                      //
		regexp.MustCompile(`(?i)<(iframe|object|embed|form|base)`), //
	} {
		if re.MatchString(html) {
			t.Errorf("chat.html が、禁止の形を含む: %s", re)
		}
	}
	for _, tag := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(tag, " src=") {
			t.Errorf("src の無い script: %s", tag)
		}
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if !strings.HasPrefix(m[1], "/static/chat") && m[1] != "/sessions" {
			t.Errorf("chat.html が、想定外の読み込み・リンクを持つ: %s", m[1])
		}
	}
}

// ページ・静的ファイルの配信: ページは requireSession・形の悪い ID は 400・serve には触れない。CSP は、既存のまま (インラインなし)。
func TestChatUIPageAndAssets(t *testing.T) {
	srv, store, _, rpID, origin := newTestServerWithRepos(t, "")
	client := loggedInClient(t, srv, store, rpID, origin)
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	page := srv.URL + "/s/" + chatTestID + "/chat"

	if resp, err := anon.Get(page); err != nil || resp.StatusCode != 401 {
		t.Errorf("無認証のページ = %v %v (401 のはず)", resp, err)
	}
	resp, err := client.Get(page) // chat.sock は無い: ページは、serve に触れず、200
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), `/static/chat.js`) {
		t.Errorf("ページ = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	for _, id := range []string{"x", "..", "20260101-000000-ABCDEF"} {
		resp, err := client.Get(srv.URL + "/s/" + id + "/chat")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("壊れた ID %q のページが返った", id)
		}
	}
	for path, ctype := range map[string]string{"/static/chat.js": "text/javascript", "/static/chat-core.js": "text/javascript", "/static/chat.css": "text/css"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) || len(b) == 0 || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s = %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	// セッション一覧から、チャットの画面への導線がある。
	if !strings.Contains(sessionsJS, "'/chat'") {
		t.Error("セッション一覧に、/s/{id}/chat へのリンクが無い")
	}
}

// chatFixtures は、golden の claude の出力 (spec/testdata/golden/claude) を、本物の変換器 (chat.Session) に通して、Event の JSON を、1 行 1 Event で dir に書く
// (node の試験が、実際の形で試せる)。
func chatFixtures(t *testing.T, dir string) {
	t.Helper()
	golden := filepath.Join("..", "..", "spec", "testdata", "golden", "claude")
	files, err := filepath.Glob(filepath.Join(golden, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("golden が無い: %v", err)
	}
	for _, f := range files {
		s := chat.NewSession(chat.SessionConfig{Launch: mustLaunch(t), ID: chatTestID, Input: io.Discard})
		if err := s.Conv.Send("hi"); err != nil { // ターンの中にする (ターンの外の権限要求は、自動で拒否される)
			t.Fatal(err)
		}
		in, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		s.ReadOutput(in)
		in.Close()
		sub, err := s.Hub.Subscribe()
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		for _, e := range sub.Snapshot {
			out.Write(e.JSON)
			out.WriteByte('\n')
		}
		sub.Close()
		s.Finish(0)
		name := strings.TrimSuffix(filepath.Base(f), ".jsonl") + ".jsonl"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(out.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// node による、表示ロジックの試験。node が無ければ skip (GORO_REQUIRE_NODE=1 なら、失敗)。
func TestChatUINode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GORO_REQUIRE_NODE") == "1" {
			t.Fatal("node が無い (GORO_REQUIRE_NODE=1)")
		}
		t.Skip("node が無い")
	}
	dir := t.TempDir()
	chatFixtures(t, dir)
	ctx := t.Context()
	cmd := exec.CommandContext(ctx, node, "--test", filepath.Join("assets", "chat", "chat.test.js"))
	cmd.Env = append(os.Environ(), "GORO_CHAT_FIXTURES="+dir)
	done := make(chan struct{})
	var out []byte
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		t.Fatal("node の試験が終わらない")
	}
	if err != nil {
		t.Fatalf("node の試験が失敗:\n%s", out)
	}
}

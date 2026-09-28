package main

// 攻撃者視点レビュー (OpenCode による独立レビュー attack-review-979253f-opencode) が見つけた・確認した、
// 端末ビュー (goro serve --repo/--session) の再現テスト。bwrap が要る。

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/termrelay/termrelaytest"
)

// TestAttackServeListenFailureHangs は、--repo で端末セッションを起動した後、net.Listen が失敗した
// とき (ポート使用中)、runServeServer が終われずにハングしないかを確かめる。修正前は、defer の登録順
// (term.Wait が defer cancel() より後に登録されるが、defer は登録順と逆に走るので、term.Wait が
// cancel より先に実行される) のせいで、term.Wait (檻の後片付けの完了を待つ。檻は ctx が取り消されて
// からしか終わらない) が無期限にブロックし、エラーを報告したままプロセスが終わらなかった
// (runServeServer が、term を起動した後の defer で、明示的に cancel を先に呼ぶよう修正済み)。
func TestAttackServeListenFailureHangs(t *testing.T) {
	f := newRunFixture(t)
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	r := f.start(t, "serve", "--listen", fmt.Sprintf("127.0.0.1:%d", port), "--rp-id", "127.0.0.1",
		"--origin", base, "--state-dir", f.stateDir(), "--repo", f.repo, "--", "termecho")
	waitStderrMatch(t, r, regexp.MustCompile(`待ち受けられない`))
	select {
	case <-r.done:
		t.Logf("listen の失敗で、プロセスは自力で終わった: %v", r.err)
	case <-time.After(10 * time.Second):
		syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
		r.wait()
		t.Fatalf("listen の失敗を報告した後、10 秒たってもプロセスが終わらない (檻が生きたままハングしている)。"+
			"stderr:\n%s", r.stderr.String())
	}
}

// TestAttackServeTerminalRawBytesAndNoLogging は、認証済み viewer が送った生バイト列 (OSC 52 風・
// OSC 8 風・NUL・不正 UTF-8 を含む) が、一切加工されずに往復し、サーバーのログ (stderr) にも出ない
// ことを確かめる (記録用。goro は中身を解釈しない、という宣言の独立検証)。
func TestAttackServeTerminalRawBytesAndNoLogging(t *testing.T) {
	f := newRunFixture(t)
	sf := startServeTermFixture(t, f, []string{"--repo", f.repo}, regexp.MustCompile(`セッション \S+ を作った`))
	ctx := t.Context()
	cookie := sf.sessionCookie(t)
	header := http.Header{"Cookie": []string{sessionCookieName + "=" + cookie}}
	cli, _, err := termrelaytest.Dial(ctx, sf.wsURL(), header)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	payload := []byte("\x1b]52;c;c2VjcmV0LWNsaXBib2FyZA==\x07" + // OSC 52 (clipboard write 風)
		"\x1b]8;;https://evil.example/\x07click\x1b]8;;\x07" + // OSC 8 (hyperlink 風)
		"nul:\x00:bad-utf8:\xff\xfe:end-probe\r\n")
	if err := cli.WriteBinary(ctx, payload); err != nil {
		t.Fatal(err)
	}
	got := termReadUntil(t, cli, "end-probe", 30*time.Second)
	if !bytes.Contains(got, payload) {
		t.Fatalf("送ったバイト列が、そのままでは返ってこない (加工されている)。\n送信: %q\n受信: %q", payload, got)
	}
	stderr := sf.r.stderr.String()
	for _, secret := range []string{"c2VjcmV0", "evil.example", "52;c;"} {
		if strings.Contains(stderr, secret) {
			t.Errorf("中継した生バイト列が、サーバーの stderr (ログ) に漏れている: %q を含む", secret)
		}
	}
}

// TestAttackServeTerminalStrongFlagsAreAbsent は、goro run の強い権限の引数 (--push・--allow・--bin)
// が、goro serve には存在しない (受理されない) ことを確かめる (見送った範囲の宣言どおりか、記録用)。
func TestAttackServeTerminalStrongFlagsAreAbsent(t *testing.T) {
	f := newRunFixture(t)
	for _, args := range [][]string{
		{"serve", "--push", "owner/repo"},
		{"serve", "--allow", "example.com"},
		{"serve", "--bin", "/bin/true"},
	} {
		r := f.goro(t, args...)
		if r.code == 0 {
			t.Errorf("goro %s が成功してしまった (強い権限の引数が使える)", strings.Join(args, " "))
		}
		if !strings.Contains(r.stderr, "flag provided but not defined") {
			t.Errorf("goro %s: 「未定義の引数」のエラーになっていない:\n%s", strings.Join(args, " "), r)
		}
	}
}

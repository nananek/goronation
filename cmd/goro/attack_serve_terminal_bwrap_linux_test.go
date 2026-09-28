package main

// 攻撃者視点レビュー (OpenCode による独立レビュー attack-review-979253f-opencode) が見つけた・確認した、
// 端末ビュー (goro serve --repo/--session) の再現テスト。bwrap が要る。goro-web-upstream への分割
// (goro serve が UDS 専用になった) にあわせて、UDS 経由で直接確かめる形に書き直した。

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/termrelay/termrelaytest"
)

// TestAttackServeListenFailureHangs は、--repo で端末セッションを起動した後、UDS の bind が失敗した
// とき (同じソケットを、別の goro serve がすでに使っている)、runServe が終われずにハングしないかを
// 確かめる。修正前は、defer の登録順 (term.Wait が defer cancel() より後に登録されるが、defer は登録順
// と逆に走るので、term.Wait が cancel より先に実行される) のせいで、term.Wait (檻の後片付けの完了を
// 待つ。檻は ctx が取り消されてからしか終わらない) が無期限にブロックし、エラーを報告したままプロセスが
// 終わらなかった (runServe が、term を起動した後の defer で、明示的に cancel を先に呼ぶよう修正済み。
// PR #36 の攻撃者視点レビューで発見・修正)。
func TestAttackServeListenFailureHangs(t *testing.T) {
	f := newRunFixture(t)
	sockPath := filepath.Join(shortDir(t), "term.sock")
	sockDir := filepath.Dir(sockPath)
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// UDS の bind (net.Listen("unix", sockPath) の前の lockDir) を、先に自分で取っておく: goro serve
	// 側の起動シーケンスは、檻を起こした後に、この lock を取ろうとして失敗する。
	lock, err := lockDir(sockDir, "test-held")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	r := f.start(t, "serve", "--state-dir", f.stateDir(), "--socket", sockPath, "--repo", f.repo, "--", "termecho")
	waitStderrMatch(t, r, regexp.MustCompile(`別の goro serve がすでに待ち受けている`))
	select {
	case <-r.done:
		t.Logf("bind の失敗で、プロセスは自力で終わった: %v", r.err)
	case <-time.After(10 * time.Second):
		syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
		r.wait()
		t.Fatalf("UDS の bind の失敗を報告した後、10 秒たってもプロセスが終わらない (檻が生きたままハングしている)。"+
			"stderr:\n%s", r.stderr.String())
	}
}

// TestAttackServeTerminalRawBytesAndNoLogging は、UDS に繋いだだけの viewer が送った生バイト列 (OSC 52
// 風・OSC 8 風・NUL・不正 UTF-8 を含む) が、一切加工されずに往復し、サーバーのログ (stderr) にも出ない
// ことを確かめる (記録用。goro は中身を解釈しない、という宣言の独立検証)。
func TestAttackServeTerminalRawBytesAndNoLogging(t *testing.T) {
	f := newRunFixture(t)
	sf := startServeTermFixture(t, f, []string{"--repo", f.repo}, regexp.MustCompile(`セッション \S+ を作った`))
	ctx := t.Context()
	cli, _, err := sf.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	termReadUntil(t, cli, "ready\n", serveTestTimeout) // raw モードに入るのを待つ (kernel の cooked echo との競合を避ける)

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

// TestAttackServeTerminalStrongFlagsAreAbsent は、goro run の強い権限の引数 (--push・--allow・--bin)・
// TCP 待ち受けの引数 (--listen・--rp-id・--origin。goro web へ移した) が、goro serve には存在しない
// (受理されない) ことを確かめる (見送った範囲・役割を絞った宣言どおりか、記録用)。
func TestAttackServeTerminalStrongFlagsAreAbsent(t *testing.T) {
	f := newRunFixture(t)
	for _, args := range [][]string{
		{"serve", "--push", "owner/repo"},
		{"serve", "--allow", "example.com"},
		{"serve", "--bin", "/bin/true"},
		{"serve", "--listen", "127.0.0.1:0"},
		{"serve", "--rp-id", "example.com"},
		{"serve", "--origin", "https://example.com"},
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

// TestAttackServeAcceptsUnauthenticatedUDSPeer は、goro serve の UDS に、goro web を経由せず直接
// 繋いでも、素通りで端末を操作できることを確かめる (仕様どおりの確認: UDS に繋げること自体が信頼の
// 境界であり、goro serve 自身は cookie も WebAuthn も持たない、という goro-web-plan の設計を、実際に
// 確かめる。ファイルシステムの権限 (0700 のディレクトリ・0600 のソケット) が、実際の防御線であることを
// あわせて確認する)。
func TestAttackServeAcceptsUnauthenticatedUDSPeer(t *testing.T) {
	f := newRunFixture(t)
	sf := startServeTermFixture(t, f, []string{"--repo", f.repo}, regexp.MustCompile(`セッション \S+ を作った`))

	fi, err := os.Stat(sf.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("UDS の権限 = %o, want 0600", perm)
	}
	if dfi, err := os.Stat(filepath.Dir(sf.sockPath)); err != nil {
		t.Fatal(err)
	} else if perm := dfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("UDS の置き場所の権限 = %o, want 0700", perm)
	}

	ctx := t.Context()
	cli, _, err := termrelaytest.Dial(ctx, "http://goro-serve.invalid/", &termrelaytest.DialOptions{HTTPClient: udsClient(sf.sockPath)})
	if err != nil {
		t.Fatalf("cookie も何も持たない接続が拒まれた (仕様と違う): %v", err)
	}
	defer cli.Close()
	termReadUntil(t, cli, "ready\n", serveTestTimeout) // raw モードに入るのを待つ (kernel の cooked echo との競合を避ける)
	const probe = "no-auth-needed\n"
	if err := cli.WriteBinary(ctx, []byte(probe)); err != nil {
		t.Fatal(err)
	}
	termReadUntil(t, cli, probe, 10*time.Second)
}

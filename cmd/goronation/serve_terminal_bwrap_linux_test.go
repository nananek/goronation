package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nananek/goronation/cmd/internal/termrelay/termrelaytest"
)

// 結合テスト (bwrap が要る): 実プロセスの goro serve (UDS 専用) が、実際の bwrap の檻の中の偽エージェント
// (termecho 場面) との間で、生バイト列を中継することを確かめる。pty 中継そのもの (SIGWINCH・raw モード
// など) は run_bwrap_linux_test.go (TestRunPtyPropagatesWinsize 等) が既に確認済みなので、ここでは
// goro serve 固有の経路 (UDS・termSession の viewer 管理) に絞る。goro serve は UDS に繋げること自体を
// 信頼の境界にするので (goro-web-plan の決定)、この結合テストは goro web を経由せず、goro serve の UDS に
// 直接繋ぐ (goro web 側の WebAuthn・reverse proxy の結合テストは web_proxy_bwrap_linux_test.go)。

const serveTestTimeout = 90 * time.Second

var serveListenRE = regexp.MustCompile(`goro serve: (\S+) で待ち受けている`)

// waitStderrMatch は、r の標準エラー出力が re に一致するまで待ち、一致部分を返す。
func waitStderrMatch(t *testing.T, r *liveRun, re *regexp.Regexp) []string {
	t.Helper()
	deadline := time.Now().Add(serveTestTimeout)
	for {
		if m := re.FindStringSubmatch(r.stderr.String()); m != nil {
			return m
		}
		select {
		case <-r.done:
			t.Fatalf("%s が出る前に、goro serve が終わった: %v\nstderr:\n%s", re, r.err, r.stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s が出ない\nstderr:\n%s", re, r.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// udsClient は、UDS の sockPath へだけ繋がる http.Client (URL の host は無視され、常に sockPath へ
// dial する)。
func udsClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
}

// serveTermFixture は、動いている goro serve (UDS 専用) と、その UDS へ繋ぐための Dial オプション。
type serveTermFixture struct {
	f        *runFixture
	r        *liveRun
	sockPath string
}

// startServeTermFixture は、goro serve (termecho 場面) を、明示的な --socket で起動する。sessionArgs は
// セッションの選び方 ({"--repo", f.repo} か {"--session", id})。sessionRE は、起動後の案内 (「を作った」
// か「を再開した」) の確認に使う。t.Cleanup で、SIGTERM を送って終了を待つ。
func startServeTermFixture(t *testing.T, f *runFixture, sessionArgs []string, sessionRE *regexp.Regexp) *serveTermFixture {
	t.Helper()
	sockPath := filepath.Join(shortDir(t), "term.sock")
	args := append([]string{"serve", "--state-dir", f.stateDir(), "--socket", sockPath}, sessionArgs...)
	args = append(args, "--", "termecho")
	r := f.start(t, args...)
	t.Cleanup(func() {
		syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
		r.wait()
	})
	waitStderrMatch(t, r, serveListenRE)
	waitStderrMatch(t, r, sessionRE)
	return &serveTermFixture{f: f, r: r, sockPath: sockPath}
}

// wsURL は、UDS 越しの WebSocket の URL (host は udsClient の DialContext が無視する)。
func (sf *serveTermFixture) wsURL() string { return "http://goro-serve.invalid/" }

func (sf *serveTermFixture) dial(ctx context.Context) (*termrelaytest.Conn, *http.Response, error) {
	return termrelaytest.Dial(ctx, sf.wsURL(), &termrelaytest.DialOptions{HTTPClient: udsClient(sf.sockPath)})
}

// TestServeTerminalRelaysRawBytes は、UDS に繋げること自体を信頼の境界にした goro serve が、生バイト列
// (中身を一切解釈・加工しない) を、実際の bwrap の檻の中のエージェントとの間で中継することを確かめる。
// 2 本目の接続による置き換え・エージェント終了後の 410 もあわせて確認する。
func TestServeTerminalRelaysRawBytes(t *testing.T) {
	f := newRunFixture(t)
	sf := startServeTermFixture(t, f, []string{"--repo", f.repo}, regexp.MustCompile(`セッション \S+ を作った`))
	ctx := t.Context()

	// (1) UDS に繋げただけの接続が、そのまま通る (認証は無い。UDS 自体が信頼の境界)。
	cli1, _, err := sf.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli1.Close()
	if err := cli1.WriteResize(ctx, 100, 40); err != nil { // resize (テキストフレーム) を混ぜても壊れない
		t.Fatal(err)
	}
	// "ready" (termecho 場面が、自分を raw モードにした後の最初の出力) を待ってから、生バイト列の
	// 確認に入る: bwrap の起動・exec の連鎖は、goro serve 自身の「待ち受け開始」の案内より遅く、この
	// viewer は間に合って attach できる (実測で確認済み)。待たずに書くと、termecho がまだ raw モードに
	// していない (kernel の cooked モードの echo が効いたままの) 短い窓に当たり、送った生バイト列が
	// kernel の echo と termecho 自身の echo で二重に返ってくることがある (テストの入力タイミングだけの
	// 問題。実運用では、人がキーを打つまでに、この窓は問題にならない)。
	readUntil(t, cli1, "ready\n")

	// (2) 送った生バイト列が、加工されずにそのまま返る (エコーの場面。goro は中身を解釈しない)。
	const probe = "hello, goro serve terminal! \x1b[31mred\x1b[0m 日本語\r\n"
	if err := cli1.WriteBinary(ctx, []byte(probe)); err != nil {
		t.Fatal(err)
	}
	readUntil(t, cli1, probe)

	// (3) 2 本目の接続は、1 本目を置き換える (「端末がついてくる」設計)。1 本目は閉じられる。
	cli2, _, err := sf.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli2.Close()
	if _, err := cli1.ReadBinary(ctx); err == nil {
		t.Error("置き換えられたはずの 1 本目が、まだ読めている")
	}
	const probe2 = "second viewer\n"
	if err := cli2.WriteBinary(ctx, []byte(probe2)); err != nil {
		t.Fatal(err)
	}
	readUntil(t, cli2, probe2)

	// (4) エージェントを終了させる (0x04 = Ctrl-D/EOT。termecho 場面の終了条件)。以後、新しい接続は 410。
	if err := cli2.WriteBinary(ctx, []byte{0x04}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(serveTestTimeout)
	for {
		_, resp, err := sf.dial(ctx)
		if err != nil {
			if resp != nil && resp.StatusCode != http.StatusGone {
				t.Errorf("セッション終了後の応答 = %d, want 410", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("セッション終了後も、新しい接続が 410 にならない")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServeTerminalResumesSession は、goro serve --session ID (前に goro run --repo が作ったセッション
// の再開) が、goro run と同じ経路 (prepareAgentLaunch・resolveRepoTarget) で正しく配線されており、
// 実際に端末ビューの WebSocket が使えることを確かめる。
func TestServeTerminalResumesSession(t *testing.T) {
	f := newRunFixture(t)
	created := f.goro(t, "run", "--repo", f.repo, "--", "exit", "0").mustOK(t)
	id := sessionID(t, created)

	sf := startServeTermFixture(t, f, []string{"--session", id}, regexp.MustCompile(`セッション \S+ を再開した`))
	ctx := t.Context()
	cli, _, err := sf.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	readUntil(t, cli, "ready\n") // raw モードに入るのを待ってから書く (kernel の cooked echo との競合を避ける)
	const probe = "resumed\n"
	if err := cli.WriteBinary(ctx, []byte(probe)); err != nil {
		t.Fatal(err)
	}
	readUntil(t, cli, probe)
}

// TestServeSocketDefaultsToTermSocketPath は、--socket を省略すると、termSocketPath (goro web が
// dial に使うのと同じ計算式) の場所で待ち受けることを確かめる (goro-web-plan §2 の決定: goro serve と
// goro web の、どちらも同じ関数で計算するので、値を受け渡す必要が無い)。
func TestServeSocketDefaultsToTermSocketPath(t *testing.T) {
	f := newRunFixture(t)
	r := f.start(t, "serve", "--state-dir", f.stateDir(), "--repo", f.repo, "--", "termecho")
	t.Cleanup(func() {
		syscall.Kill(r.cmd.Process.Pid, syscall.SIGTERM)
		r.wait()
	})
	m := waitStderrMatch(t, r, serveListenRE)
	waitStderrMatch(t, r, regexp.MustCompile(`セッション \S+ を作った`))
	gotPath := m[1]

	sess := waitStderrMatch(t, r, regexp.MustCompile(`\(session=(\S+)\)`))
	want := termSocketPath(f.stateDir(), defaultGroup, sess[1])
	if gotPath != want {
		t.Errorf("既定の UDS の path = %q, want %q (termSocketPath と同じ計算式)", gotPath, want)
	}

	client := udsClient(want)
	ctx := t.Context()
	cli, _, err := termrelaytest.Dial(ctx, "http://goro-serve.invalid/", &termrelaytest.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("既定の path に繋げない: %v", err)
	}
	cli.Close()
}

// TestServeVersionEndpoint は、GET /version (UDS 越し) が、認証を問わず (UDS に繋げること自体が信頼の
// 境界) 妥当なビルド情報を返すことを確かめる。goro web と goro serve は別プロセス・別ライフサイクル
// (片方だけ再起動できる) なので、将来のバージョン食い違いに備えて用意した endpoint (goro-web-plan)。
// この版では、値を返すところまでで、goro web 側での不一致検知はまだ作り込まない。
func TestServeVersionEndpoint(t *testing.T) {
	f := newRunFixture(t)
	sf := startServeTermFixture(t, f, []string{"--repo", f.repo}, regexp.MustCompile(`セッション \S+ を作った`))

	resp, err := udsClient(sf.sockPath).Get("http://goro-serve.invalid/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/version = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("/version の Content-Type = %q, want application/json", ct)
	}
	var v versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("/version の応答が JSON として読めない: %v", err)
	}
	if v.GoVersion == "" {
		t.Error("goVersion が空")
	}
}

// readUntil は、cli から読み続け、連結した内容が want を含むまで待つ (境界がバイト単位で保証されない
// ため、1 回の ReadBinary が want と一致するとは限らない)。
func readUntil(t *testing.T, cli *termrelaytest.Conn, want string) {
	t.Helper()
	var got strings.Builder
	deadline := time.Now().Add(serveTestTimeout)
	for !strings.Contains(got.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%q が届かない (got %q)", want, got.String())
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		b, err := cli.ReadBinary(ctx)
		cancel()
		if err != nil {
			continue
		}
		got.Write(b)
	}
}

// termReadUntil は、readUntil と同じことを、蓄積したバイト列 ([]byte) をそのまま返す形でする
// (攻撃者視点レビューの再現テストが、受け取った生バイト列そのものを検査するのに使う)。
func termReadUntil(t *testing.T, cli *termrelaytest.Conn, want string, timeout time.Duration) []byte {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(string(got), want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q が届かない (got %q)", want, got)
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		b, err := cli.ReadBinary(ctx)
		cancel()
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		got = append(got, b...)
	}
}

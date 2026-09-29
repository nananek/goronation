//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nananek/goronation/cmd/framecapture/internal/connectproxy"
	"github.com/nananek/goronation/sandbox/bwrap"
	"github.com/nananek/goronation/tools/fakeproviders/anthropic"
)

// runClaudeInteractive は、PR⓪ スパイク (Issue #1) 用の対話 (host モード) 採取: p.sc.ClaudePermissions が
// 1 つ以上あるときに runClaude が使う経路。既存の runHarness (全ターンを起動前に書いて stdin を閉じる、
// 静的な経路) とは違い、stdin/stdout を pipe で直結し、claude が出す can_use_tool の control_request を
// 読みながら答え、ターンの result を見てから次のターンを書く (追いプロンプト。stdin は最後まで閉じない)。
//
// 依存の生成 (fake サーバー・connectproxy・cageConfig) は runHarness と重複するが、意図的に共有しない:
// runHarness は全ターン事前書き込み・stdin クローズの単純さを保ち (既存の golden fixtures の再現性に効く)、
// こちらは対話専用の別経路として独立させる (PR⓪ はスパイクであり、共通化は、この経路の形が固まってから
// 後続 PR で検討する)。
func runClaudeInteractive(o captureOptions, p plan, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "framecapture claude: "+format+"\n", a...)
		return 1
	}

	goroExe, err := findBin(o.goronationBin, "goronation")
	if err != nil {
		return fail("%v", err)
	}
	agentExe, err := findBin(o.agentBin, "claude")
	if err != nil {
		return fail("%v", err)
	}

	stateDir, err := os.MkdirTemp("", "framecapture-")
	if err != nil {
		return fail("作業ディレクトリを作れない: %v", err)
	}
	defer os.RemoveAll(stateDir)

	home := filepath.Join(stateDir, "home")
	work := filepath.Join(stateDir, "work")
	runDir := filepath.Join(stateDir, "run")
	for _, d := range []string{home, work, runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fail("%s を作れない: %v", d, err)
		}
	}
	if p.sc != nil {
		if err := p.sc.writeFiles(work); err != nil {
			return fail("work の下ごしらえに失敗: %v", err)
		}
	}

	cert, err := newSelfSignedCert(claudeFakeHost)
	if err != nil {
		return fail("自己署名証明書を作れない: %v", err)
	}
	caPath := filepath.Join(stateDir, "ca.pem")
	if err := os.WriteFile(caPath, cert.CAPEM, 0o600); err != nil {
		return fail("CA 証明書を書けない: %v", err)
	}

	steps := p.sc.Claude
	if len(steps) == 0 {
		return fail("%v", errNoSteps)
	}
	fake, err := startFakeTLS(cert, anthropic.NewServer(steps...).NewHTTPServer())
	if err != nil {
		return fail("fake サーバーを起動できない: %v", err)
	}
	defer fake.Close()

	sockPath := filepath.Join(runDir, proxySockName)
	pl, err := net.Listen("unix", sockPath)
	if err != nil {
		return fail("connectproxy の UDS で待ち受けられない: %v", err)
	}
	target := fmt.Sprintf("%s:%d", claudeFakeHost, claudeFakeVirtualPort)
	real := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(fake.Port))
	proxy := connectproxy.New(map[string]netip.AddrPort{target: real})
	proxy.Logf = func(format string, args ...any) {
		fmt.Fprintf(stderr, "framecapture claude: connectproxy: "+format+"\n", args...)
	}
	proxyErr := make(chan error, 1)
	go func() { proxyErr <- proxy.Serve(pl) }()
	defer func() {
		proxy.Close()
		<-proxyErr
	}()

	// stdin/stdout を、静的な Reader/Writer ではなく実 pipe (os.Pipe) にする: *os.File は os/exec が
	// fd を直結するだけで中継 goroutine を作らないので、こちらが EOF のタイミング (子の側の fd が
	// 閉じたとき) を自分で制御できる (子側の複製 (stdinR・stdoutW) を Start 後すぐ閉じるのが必須。
	// さもないと親のプロセスが最後の書き手/読み手として残り続け、相手側が EOF を見られない)。
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fail("stdin の pipe を作れない: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fail("stdout の pipe を作れない: %v", err)
	}

	host := bwrap.CurrentHost()
	cfg := cageConfig{
		Host: host, AgentExe: agentExe, AgentBinName: filepath.Base(agentExe), GoroExe: goroExe,
		CACertPEM: caPath, RunDir: runDir, Home: home, Work: work,
		Env: claudeEnv(), Args: claudeInteractiveArgs(), Stdout: stdoutW, Stderr: stderr,
	}
	spec := cageSpec(cfg)
	spec.Stdin = stdinR

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if o.timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, o.timeout)
		defer cancelTimeout()
	}

	c, err := bwrap.Start(ctx, spec)
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return fail("檻を起動できない: %v", err)
	}
	// 子 (bwrap、ひいては claude) 側の複製を閉じる。手元に残すのは、書く側 (stdinW)・読む側 (stdoutR) だけ。
	stdinR.Close()
	stdoutW.Close()

	var raw bytes.Buffer
	convErr := driveClaudeConversation(p.turns, p.sc.ClaudePermissions, stdinW, io.TeeReader(stdoutR, &raw))
	stdinW.Close() // 会話を終えたら stdin を閉じる (手動の「終了」に相当。まだ閉じていなければ)。
	stdoutR.Close()
	if convErr != nil {
		fmt.Fprintf(stderr, "framecapture claude: 対話の駆動で問題があった (フレームはここまで採れている): %v\n", convErr)
	}

	waitErr := c.Wait()
	code := exitCodeOf(waitErr, stderr)
	return finish(o, "claude", p, code, raw.Bytes(), stdout, stderr)
}

// claudeInteractiveArgs は、claudeArgs() に、対話 (host モード) の採取に要る引数を足したもの。
//
// --permission-prompt-tool stdio は、--help には出ない隠しフラグ (PR⓪ スパイク (Issue #1) の所見):
// 既定の --permission-prompts host のままでは、can_use_tool の control_request は一切出ず、非対話のときと
// 同じ即時 permission_denied になる (stdin/stdout の control channel 自体は機能し、client 発の
// control_request (subtype "initialize") には control_response で答えるが、can_use_tool だけは出ない)。
// @anthropic-ai/claude-agent-sdk 0.3.284 の core.mjs を読んで見つけた (canUseTool コールバックを渡すと、
// SDK はこの引数を足す。--permission-prompts はこの経路では付けなくてよい、既定のままでよい)。
func claudeInteractiveArgs() []string {
	return append(append([]string{}, claudeArgs()...), "--permission-prompt-tool", "stdio")
}

// controlRequestFrame は、stream-json の 1 行のうち、この spike が読み取る部分だけを取り出す最小限の形。
type controlRequestFrame struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"` // "result" 判定用 (control_request 行では使わない)
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	} `json:"request"`
}

// driveClaudeConversation は、turns を順に stdin に書き、can_use_tool の control_request に answers で
// 順に答え (尽きたら最後を繰り返す)、各ターンの result を見てから次の turn を書く (turns を使い切ったら
// 何もせず読み続け、プロセス側の出力が尽きる=EOF まで待つ)。stdout の読み取りが EOF で終わるまでブロックする
// (呼び手が context の期限で檻を止めれば、EOF で戻る)。
func driveClaudeConversation(turns []string, answers []claudePermissionAnswer, stdin io.Writer, stdout io.Reader) error {
	if len(turns) == 0 {
		return nil
	}
	// client 発の control_request (subtype "initialize") を、最初のターンの前に送る (フィールドは
	// すべて任意なので空でよい)。can_use_tool が出るかどうかは、実際には claudeInteractiveArgs() の
	// --permission-prompt-tool stdio が決めている (その doc comment 参照) が、initialize は実運用の
	// クライアント (SDK) が必ず送るものなので、この spike でも同じ手順を踏む。
	if err := writeLine(stdin, map[string]any{
		"type":       "control_request",
		"request_id": "spike-init",
		"request":    map[string]any{"subtype": "initialize"},
	}); err != nil {
		return fmt.Errorf("initialize control_request を書けない: %w", err)
	}
	if err := writeLine(stdin, claudeStreamJSONLine(turns[0])); err != nil {
		return fmt.Errorf("1 ターン目を書けない: %w", err)
	}
	nextTurn := 1
	permIdx := 0
	nextAnswer := func() (claudePermissionAnswer, bool) {
		if len(answers) == 0 {
			return claudePermissionAnswer{}, false
		}
		i := permIdx
		if i >= len(answers) {
			i = len(answers) - 1
		}
		permIdx++
		return answers[i], true
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var f controlRequestFrame
		if err := json.Unmarshal(line, &f); err != nil {
			continue // フレームの形を見るだけなので、ここでの unmarshal 失敗は無視する (raw には既に残っている)。
		}
		switch {
		case f.Type == "control_request" && f.Request.Subtype == "can_use_tool":
			ans, ok := nextAnswer()
			if !ok {
				ans = claudePermissionAnswer{Outcome: "allow"} // 場面が answers を用意し忘れたときの既定 (安全側は deny だが、スパイクでは検出しやすい allow を既定にする)。
			}
			switch ans.Outcome {
			case "deny":
				if err := writeLine(stdin, controlResponseDeny(f.RequestID, ans.Message)); err != nil {
					return err
				}
			case "interrupt":
				if err := writeLine(stdin, controlRequestInterrupt("spike-interrupt-1")); err != nil {
					return err
				}
				if err := writeLine(stdin, controlCancelRequest(f.RequestID)); err != nil {
					return err
				}
			default: // "allow" を含め、既定は allow
				if err := writeLine(stdin, controlResponseAllow(f.RequestID)); err != nil {
					return err
				}
			}
		case f.Type == "result":
			if nextTurn < len(turns) {
				if err := writeLine(stdin, claudeStreamJSONLine(turns[nextTurn])); err != nil {
					return fmt.Errorf("%d 番目のターンを書けない: %w", nextTurn+1, err)
				}
				nextTurn++
			} else {
				// 最後のターンの result を見た。stdin はまだ開いたままだと claude は次の入力を
				// 待ち続け (production の「終了」ボタンに相当する操作が無いと戻ってこない)、ここで
				// 読むのをやめないと呼び手 (runClaudeInteractive) が stdin を閉じる機会が来ず、
				// タイムアウトまで固まる。呼び手はこの後すぐ stdin を閉じて (手動の「終了」に相当)、
				// claude の終了を待つ。
				return sc.Err()
			}
		}
	}
	return sc.Err()
}

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// controlResponseAllow は、can_use_tool の control_request request_id への、許可の control_response。
func controlResponseAllow(requestID string) map[string]any {
	return map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   map[string]any{"behavior": "allow"},
		},
	}
}

// controlResponseDeny は、can_use_tool の control_request request_id への、拒否の control_response
// (message は、tool_result に載る、人間向けの却下理由)。
func controlResponseDeny(requestID, message string) map[string]any {
	return map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   map[string]any{"behavior": "deny", "message": message},
		},
	}
}

// controlRequestInterrupt は、host (この harness) 発の control_request: 進行中のターンを中断する
// (subtype "interrupt")。requestID は、この control_request 自身の request_id (claude からの
// control_response が echo する)。
func controlRequestInterrupt(requestID string) map[string]any {
	return map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    map[string]any{"subtype": "interrupt"},
	}
}

// controlCancelRequest は、pendingRequestID (未決の can_use_tool の request_id) への
// control_cancel_request: もう答えを必要としないことを伝える。
func controlCancelRequest(pendingRequestID string) map[string]any {
	return map[string]any{
		"type":       "control_cancel_request",
		"request_id": pendingRequestID,
	}
}

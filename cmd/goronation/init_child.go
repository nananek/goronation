package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// --relay-control のときの、子の起動まわり (ADR 0020・0029・0030)。

const (
	// legacyTokenEnv は、opencode の旧名のパスワードの環境変数 (子の環境から消す)。
	legacyTokenEnv = "OPENCODE_SERVER_PASSWORD"

	// versionCheckTimeout・versionCheckMax は、子の --version の実行の時間と出力の上限。
	versionCheckTimeout = 10 * time.Second
	versionCheckMax     = 4 << 10

	// startupReasonMax は、標準出力の {"error":…} の理由の長さの上限 (バイト)。
	startupReasonMax = 300
)

// selfLaunch は、自分 (goronation) を、いま起動されたのと同じ形で、別のサブコマンドのために起動する argv の前半を返す
// (実行ファイルと、サブコマンド (init) の前の引数。通常は実行ファイルだけ)。
func selfLaunch() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	rest := os.Args[1:]
	i := slices.Index(rest, "init")
	if i < 0 {
		return nil, errors.New("自分の起動の引数に init が無い")
	}
	return append([]string{exe}, rest[:i]...), nil
}

// landlockWrap は、argv (絶対 path の実行ファイルと引数) を、goronation landlock-exec --allow-connect ports -- で包んだ、実行ファイルと引数を返す。
func landlockWrap(ports string, argv []string) (string, []string, error) {
	self, err := selfLaunch()
	if err != nil {
		return "", nil, err
	}
	return self[0], append(append(self[1:], "landlock-exec", "--allow-connect", ports, "--"), argv...), nil
}

// withoutEnv は、env から、names の名前の変数を全部消した、新しい slice を返す。
func withoutEnv(env []string, names ...string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

// capWriter は、先頭 max バイトだけ貯め、残りは捨てる (書き手は止めない)。
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// checkChildVersion は、name args (子の --version。stdin は空・環境は env) を実行し、標準出力の先頭が prefix で始まることを確かめる。
// 時間と出力の大きさに上限がある。
func checkChildVersion(name string, args, env []string, prefix string) error {
	ctx, cancel := context.WithTimeout(context.Background(), versionCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, errOut := &capWriter{max: versionCheckMax}, &capWriter{max: versionCheckMax}
	cmd.Env, cmd.Stdout, cmd.Stderr = env, out, errOut
	cmd.WaitDelay = time.Second // 孫が pipe を握っていても、待ち続けない
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("--version を実行できない: %w (%s)", err, shorten(errOut.buf.String(), 120))
	}
	if !strings.HasPrefix(out.buf.String(), prefix) {
		return fmt.Errorf("--version の出力が %q で始まらない: %q", prefix, shorten(out.buf.String(), 80))
	}
	return nil
}

// shorten は、s を、空白を詰めて、max バイト以内の、先頭の部分にする。
func shorten(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// reportStartup・reportReady は、init の起動の結果を、標準出力に 1 行だけ出す (ADR 0030)。
func reportStartup(w io.Writer, reason string) {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{shorten(reason, startupReasonMax)})
	w.Write(append(b, '\n'))
}

func reportReady(w io.Writer) { io.WriteString(w, "{\"ready\":true}\n") }

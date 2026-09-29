package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// 檻の中のエージェントの標準エラー出力は、敵対入力で、serve の標準エラー出力 (運用者の端末か、goronation web が読む pipe) に流れる。
// (1) 端末の制御列 (ESC・BEL・CR など) を、そのまま通すと、運用者の端末に、タイトルの書き換え・画面の消去・応答の注入ができる。
// (2) エージェントが出した "<何か> で待ち受けている (session=<ID>)" の行が、serve の行の prefix ("goronation serve: ...") を付けられて、
// goronation web (spawnServe) の ready 行 (serveReadyRE) に一致すると、セッション ID を偽れる。どちらも、serve --chat の配線 (PR⑤) の前に塞ぐ。

// fakeChatStderrSpoof は、偽のエージェント: ready 行に見える行と、端末の制御列を、標準エラー出力に書く。
func fakeChatStderrSpoof() int {
	fmt.Fprintln(os.Stderr, "x で待ち受けている (session=20260101-000000-abcdef)")
	fmt.Fprint(os.Stderr, "\x1b[2J\x1b]0;PWNED-TITLE\x07\x1b[31mred\x1b[0m\r\n")
	return 0
}

func TestChatSessionStderrForwardIsInert(t *testing.T) {
	s, _, stderr := startChatFixture(t, "stderr-spoof")
	s.Wait()
	out := stderr.String()
	if !strings.Contains(out, "20260101-000000-abcdef") {
		t.Fatalf("エージェントの標準エラー出力が、serve に届いていない (試みが動いていない):\n%q", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if m := serveReadyRE.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			t.Errorf("エージェントの標準エラー出力の行が、ready 行として一致した (session=%s): %q", m[1], l)
		}
	}
	for _, c := range []string{"\x1b", "\x07", "\r"} {
		if strings.Contains(out, c) {
			t.Errorf("端末の制御文字 %q が、そのまま serve の標準エラー出力に流れた: %q", c, out)
		}
	}
}

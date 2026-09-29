package main

import (
	"bytes"
	"fmt"
	"github.com/nananek/goronation/cmd/internal/chat"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// root では、chat を起こさない (檻に capability が残り、標準入出力を奪われる)。root でなければ、この経路は結合テストが確かめる。
func TestStartChatSessionRefusesRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root でだけ確かめる")
	}
	_, err := startChatSession(t.Context(), "id", cageConfig{}, nil, mustLaunch(t), nil, t.TempDir(), &bytes.Buffer{})
	if err != errChatRoot {
		t.Fatalf("err = %v", err)
	}
}

// chat の檻: goronation init は PID 1 (--as-pid-1) で、--non-dumpable が付く。付けない檻 (これまでの形) には、どちらも出ない。
func TestCageSpecNonDumpable(t *testing.T) {
	argv, err := cageSpec(testCage()).Argv()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range argv {
		if a == "--as-pid-1" || a == "--non-dumpable" {
			t.Errorf("既定の檻に %s が出ている", a)
		}
	}
	c := testCage()
	c.NonDumpable = true
	argv, err = cageSpec(c).Argv()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "--as-pid-1") {
		t.Errorf("--as-pid-1 が無い: %q", argv)
	}
	i := slices.Index(argv, "--non-dumpable")
	j := slices.Index(argv, "--")
	if i < 0 || slices.Index(argv, "init") > i || slices.Index(argv[j+1:], "--")+j+1 < i {
		t.Errorf("--non-dumpable は、init の引数 (子のコマンドの前) に出るはず: %q", argv)
	}
}

func TestInitParsesNonDumpable(t *testing.T) {
	cfg, err := parseInitArgs([]string{"--listen", "127.0.0.1:0", "--upstream", "/run/x.sock", "--non-dumpable", "--", "c"}, &bytes.Buffer{})
	if err != nil || !cfg.nonDump {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if cfg, _ := parseInitArgs([]string{"--listen", "127.0.0.1:0", "--upstream", "/run/x.sock", "c"}, &bytes.Buffer{}); cfg.nonDump {
		t.Error("既定で nonDump が立っている")
	}
}

// 読めない複製: mode 0100・中身は同じ・同じ実体なら作り直さない・古い複製は消す・実体が通常のファイルでなければ断る。
func TestUnreadableExeCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "agent")
	if err := os.WriteFile(src, []byte("#!agent-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copies")
	p1, err := unreadableExeCopy(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p1)
	if err != nil || fi.Mode() != 0o100 {
		t.Fatalf("複製 = %v err=%v (mode 0100 のはず)", fi, err)
	}
	if os.Geteuid() != 0 { // root は、mode を無視して読める
		if _, err := os.ReadFile(p1); err == nil {
			t.Error("複製を読めた")
		}
	}
	p2, err := unreadableExeCopy(dst, src)
	if err != nil || p2 != p1 {
		t.Fatalf("同じ実体で、別の複製: %q → %q (%v)", p1, p2, err)
	}
	// 実体が変わったら (大きさ・更新時刻)、新しい複製を作り、古い複製を消す。
	if err := os.WriteFile(src, []byte("#!agent-bytes-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	p3, err := unreadableExeCopy(dst, src)
	if err != nil || p3 == p1 {
		t.Fatalf("実体が変わったのに、同じ複製: %q (%v)", p3, err)
	}
	// 猶予の間は、古い複製 (別の呼び手が、いま使っているかもしれない) を消さない (L1)。
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("猶予の間に、古い複製を消した: %v", err)
	}
	// 猶予を過ぎた古い複製は、次の呼び出しで消える。
	old := time.Now().Add(-2 * exeCopyGrace)
	if err := os.Chtimes(p1, old, old); err != nil {
		t.Fatal(err)
	}
	if p4, err := unreadableExeCopy(dst, src); err != nil || p4 != p3 {
		t.Fatalf("p4 = %q (%v)", p4, err)
	}
	if _, err := os.Stat(p1); err == nil {
		t.Error("猶予を過ぎた古い複製が残っている")
	}
	ents, _ := os.ReadDir(dst)
	for _, e := range ents {
		if e.Name() != filepath.Base(p3) && e.Name() != ".lock" {
			t.Errorf("余計なファイル: %s", e.Name())
		}
	}
	if _, err := unreadableExeCopy(dst, dir); err == nil {
		t.Error("ディレクトリを、複製できた")
	}
	if _, err := unreadableExeCopy(dst, filepath.Join(dir, "none")); err == nil {
		t.Error("無いファイルを、複製できた")
	}
}

// 標準エラー出力は、上限までを、行頭に接頭辞を付けて残し、超えた分は捨てる (書いたことにして、エージェントを止めない)。
func TestCappedWriter(t *testing.T) {
	var out bytes.Buffer
	w := &cappedWriter{w: &out, prefix: "> ", left: 12}
	if n, err := w.Write([]byte("abc\ndef")); n != 7 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := w.Write([]byte("ghi\njkl\nmno\n")); n != 12 || err != nil {
		t.Fatal(n, err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "> abc\n> defg") || strings.Contains(got, "mno") {
		t.Errorf("出力 = %q", got)
	}
	if n, _ := w.Write([]byte("more\n")); n != 5 || strings.Contains(out.String(), "more") {
		t.Error("上限の後も、書いている")
	}
}

func mustLaunch(t *testing.T) chat.Launch {
	t.Helper()
	l, err := chat.Agent("claude")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// 版の違う実体を並行に呼んでも、返した path が消えない (L1)。全員が返った後も、全部の path がある。
func TestUnreadableExeCopyConcurrentVersions(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "copies")
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		paths := make([]string, 4)
		for i := range paths {
			src := filepath.Join(dir, fmt.Sprintf("agent%d", i))
			if err := os.WriteFile(src, []byte(strings.Repeat("x", 10+i+round)), 0o755); err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := unreadableExeCopy(dst, src)
				if err != nil {
					t.Error(err)
				}
				paths[i] = p
			}()
		}
		wg.Wait()
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("round %d: 返した path が、もう無い: %v", round, err)
			}
		}
	}
}

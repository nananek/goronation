package main

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

func TestOpenPty(t *testing.T) {
	master, slave, err := openHostPty()
	if err != nil {
		t.Skipf("openPty: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if _, err := master.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := slave.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "hello\n" {
		t.Errorf("slave が読んだもの = %q, want %q", got, "hello\n")
	}
}

func TestGetSetWinsize(t *testing.T) {
	master, slave, err := openHostPty()
	if err != nil {
		t.Skipf("openPty: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	want := winsize{Row: 40, Col: 120, Xpixel: 800, Ypixel: 600}
	if err := setWinsize(master, want); err != nil {
		t.Fatalf("setWinsize: %v", err)
	}
	got, err := getWinsize(slave)
	if err != nil {
		t.Fatalf("getWinsize: %v", err)
	}
	if got != want {
		t.Errorf("getWinsize(slave) = %+v, want %+v (master と slave は、大きさを共有するはず)", got, want)
	}
}

// TestPtyRelayCopiesBothDirections は、stdin→master・master→stdout の両方向に、バイト列がそのまま届くことを確認する。
func TestPtyRelayCopiesBothDirections(t *testing.T) {
	master, slave, err := openHostPty()
	if err != nil {
		t.Skipf("openPty: %v", err)
	}
	defer slave.Close()

	// エージェント役の slave を raw モードにする (実際のエージェントが、自分の端末に対して行うのと同じ)。
	// でないと、既定 (canonical・ECHO オン) のせいで、slave への入力が、そのまま master 側に echo され
	// (CRLF 変換つきで)、中継のバイト列の確認が、echo のノイズと混ざる。
	setTermOf(t, slave, rawOf(termOf(t, slave)))

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinW.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()

	r := startPtyRelay(stdinR, stdoutW, master)
	defer r.stop()

	// ホスト (fake stdin) → master → slave (エージェント側)。
	if _, err := stdinW.WriteString("to-agent\n"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 32)
	if err := slave.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := slave.Read(got)
	if err != nil {
		t.Fatalf("slave が stdin からの中継を読めない: %v", err)
	}
	if string(got[:n]) != "to-agent\n" {
		t.Errorf("slave が読んだもの = %q, want %q", got[:n], "to-agent\n")
	}

	// slave (エージェント側) → master → stdout (ホスト)。
	if _, err := slave.WriteString("to-host\n"); err != nil {
		t.Fatal(err)
	}
	if err := stdoutR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err = stdoutR.Read(got)
	if err != nil {
		t.Fatalf("stdout が master からの中継を読めない: %v", err)
	}
	if string(got[:n]) != "to-host\n" {
		t.Errorf("stdout が読んだもの = %q, want %q", got[:n], "to-host\n")
	}
}

// TestPtyRelayStopDoesNotHang は、stop が、master→stdout の中継の終了を待って、すぐ戻ることを確認する
// (stdin→master 側は、実端末からの次の入力を待つ可能性があるので、待たない。ファイル冒頭のコメントを参照)。
func TestPtyRelayStopDoesNotHang(t *testing.T) {
	master, slave, err := openHostPty()
	if err != nil {
		t.Skipf("openPty: %v", err)
	}
	defer slave.Close()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinW.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()

	r := startPtyRelay(stdinR, stdoutW, master)

	done := make(chan struct{})
	go func() { r.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop が戻らない (stdin からの読みを待ち続けている)")
	}

	// master はもう閉じているので、slave への書き込みは、いずれ失敗する (エラーは確認しない。相手が消えた合図)。
	_, _ = io.Copy(io.Discard, bytes.NewReader(nil))
}

// TestPtyRelayAppliesInitialWinsize は、startPtyRelay が、stdin の大きさを、開始時に master (と slave) へ反映することを確認する。
func TestPtyRelayAppliesInitialWinsize(t *testing.T) {
	stdinM, stdinS, err := openHostPty() // stdin 役の pty: 実端末の代わり
	if err != nil {
		t.Skipf("openPty: %v", err)
	}
	defer stdinM.Close()
	defer stdinS.Close()
	if err := setWinsize(stdinS, winsize{Row: 24, Col: 100}); err != nil {
		t.Fatal(err)
	}

	master, slave, err := openHostPty()
	if err != nil {
		t.Skipf("openPty: %v", err)
	}
	defer slave.Close()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()
	defer stdoutW.Close()

	r := startPtyRelay(stdinS, stdoutW, master)
	defer r.stop()

	got, err := getWinsize(slave)
	if err != nil {
		t.Fatal(err)
	}
	if got.Row != 24 || got.Col != 100 {
		t.Errorf("開始時の大きさ = %+v, want Row=24 Col=100", got)
	}
}

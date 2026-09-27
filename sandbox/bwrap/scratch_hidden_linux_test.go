//go:build linux

package bwrap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/goronation/core/sandbox"
	"github.com/nananek/goronation/sandbox/contract"
)

// TestScratchNeverPersistsOnHost は、契約の「Scratch は、檻専用・揮発の一時ディレクトリ。ホストに残らない」が、Write の下に置いた Scratch でも成り立つことを確認する。
// bwrap は tmpfs を、bind より先に作る。Scratch を Write の下に置くと、後から bind する Write が tmpfs を隠し、檻が Scratch に書いたものは、ホストの Write に残る。
// 直し方は、Spec を断る (ErrRejected) でも、並べ替えでもよい: 断るか、ホストに残さないかの、どちらか。
func TestScratchNeverPersistsOnHost(t *testing.T) {
	needBwrap(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(work, "scr"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := sandbox.Spec{
		Exec: "/usr/bin/sh", Args: []string{"-c", "echo x > /work/scr/leak.txt"}, System: true,
		Write:   []sandbox.Mount{{HostPath: work, GuestPath: "/work"}},
		Scratch: []string{"/work/scr"},
		Stdout:  &out, Stderr: &out,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cage, err := New(contract.Host{Home: filepath.Join(root, "home")}).Start(ctx, s)
	if errors.Is(err, sandbox.ErrRejected) {
		return // Scratch を Write の下に置けない、と断るのは、契約どおり
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = cage.Wait()
	if _, err := os.Stat(filepath.Join(work, "scr", "leak.txt")); err == nil {
		t.Errorf("Scratch (/work/scr) に書いたものが、ホストの %s に残った (契約: ホストに残らない)。檻の出力: %s", filepath.Join(work, "scr", "leak.txt"), out.String())
	}
}

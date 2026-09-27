//go:build linux

package main

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/goronation/core/credential"
	"github.com/nananek/goronation/egress/git"
)

// fakeCredSource は、テストだけで使う credential.Source (檻には渡らない。startProxy に渡すためだけの偽物)。
type fakeCredSource map[string]credential.Secret

func (f fakeCredSource) Token(_ context.Context, name string) (credential.Secret, error) {
	if v, ok := f[name]; ok {
		return v, nil
	}
	return credential.Secret{}, credential.ErrNotFound
}

// dialStatusLine は、runDir の UDS に繋ぎ、line (+ "\r\n\r\n") を送って、応答の状態行を返す。
func dialStatusLine(t *testing.T, runDir, line string) string {
	t.Helper()
	c, err := net.DialTimeout("unix", filepath.Join(runDir, proxySockName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(line + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// TestStartProxyPushRoutesGitPath は、push が非 nil のとき、git.PathPrefix の要求が (CONNECT としてではなく)
// gateway.Handler に届くことを確かめる: 許可していない repo (push が指す repo と違う) への要求は、gateway 自身の
// Policy.CheckRepo が 403 で断る。これは、この経路が実際に gateway まで届いた証拠になる (アップストリームには、
// 一切繋ぎに行かない)。
func TestStartProxyPushRoutesGitPath(t *testing.T) {
	runDir := shortDir(t)
	policy, err := git.NewPolicy(git.Repo{Owner: "o", Name: "allowed-repo"}, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	push := &pushConfig{Policy: policy, Credentials: fakeCredSource{}}
	p, err := startProxy(runDir, nil, push)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	status := dialStatusLine(t, runDir, "GET /git/github.com/o/other-repo.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: x")
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("許可外 repo への git 要求の status = %q, want 403 (gateway の Policy.CheckRepo が断ったはず)", status)
	}

	// CONNECT は、これまでどおり動く (push を配線しても、CONNECT 経路は変わらない)。
	status = dialStatusLine(t, runDir, "CONNECT not-allowed.invalid:443 HTTP/1.1")
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("CONNECT (許可リストに無い宛先) の status = %q, want 403", status)
	}
}

// TestStartProxyNilPushRejectsGitPath は、push が nil のとき (--push を指定しないとき)、git.PathPrefix 宛の
// 要求が、CONNECT 以外として 405 で断られることを確かめる (これまでどおりの、CONNECT だけの egress.Server.Serve)。
func TestStartProxyNilPushRejectsGitPath(t *testing.T) {
	runDir := shortDir(t)
	p, err := startProxy(runDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	status := dialStatusLine(t, runDir, "GET /git/github.com/o/r.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: x")
	if !strings.HasPrefix(status, "HTTP/1.1 405") {
		t.Fatalf("push が nil のときの git 要求の status = %q, want 405 (CONNECT 以外は method で断るはず)", status)
	}
}

// TestNewPushConfig は、newPushConfig が、正しい repo・session から git.Policy を作り、壊れた session ID
// (git.NewPolicy の検査を通らない) では error になることを確かめる。
func TestNewPushConfig(t *testing.T) {
	dir := shortDir(t)
	push, err := newPushConfig(dir, "o/r", "sess-1")
	if err != nil {
		t.Fatalf("newPushConfig: %v", err)
	}
	if push.Policy.Repo.Owner != "o" || push.Policy.Repo.Name != "r" || push.Policy.Session != "sess-1" {
		t.Fatalf("Policy = %+v", push.Policy)
	}
	if _, err := newPushConfig(dir, "o/r", "../escape"); err == nil {
		t.Fatal("壊れた session ID なのに error にならなかった")
	}
	if _, err := newPushConfig(dir, "not-a-repo", "sess-1"); err == nil {
		t.Fatal("壊れた repo なのに error にならなかった")
	}
}

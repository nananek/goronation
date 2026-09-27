package conformance

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/goronation/sandbox/contract"
)

// fixture は、適合テストが使う、ホスト側の準備: 偽の HOME (資格情報の偽物を持つ)・見せる dir・見せない dir・probe の実行ファイル。
//
//	root/
//	  home/            偽のホストの HOME (.ssh・.claude・.gnupg・.aws・.config/gh・.netrc・.git-credentials・notes.txt)
//	  sockdir/agent.sock  SSH_AUTH_SOCK に見立てたファイル (Host.Secrets)
//	  other/f.txt      見せない dir
//	  granted/{bin/probe, work, ro, run}  見せる dir
type fixture struct {
	root, home, sockDir, sock, other string
	work, ro, run, exe               string
	marker                           string
	homeFiles                        []string
	host                             contract.Host
}

// newFixture は、短い path の一時ディレクトリ (UDS の path の上限に収まる。symlink は辿った実体) に、fixture を作る。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{root: root, marker: fmt.Sprintf("GORO-CONFORMANCE-SECRET-%d", time.Now().UnixNano())}
	fx.home = filepath.Join(root, "home")
	fx.sockDir = filepath.Join(root, "sockdir")
	fx.sock = filepath.Join(fx.sockDir, "agent.sock")
	fx.other = filepath.Join(root, "other")
	granted := filepath.Join(root, "granted")
	fx.work, fx.ro, fx.run = filepath.Join(granted, "work"), filepath.Join(granted, "ro"), filepath.Join(granted, "run")
	write := func(p, content string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{".ssh/id_ed25519", ".claude/.credentials.json", ".claude.json", ".gnupg/private-keys-v1.d/k.key",
		".aws/credentials", ".config/gh/hosts.yml", ".netrc", ".git-credentials", "notes.txt"} {
		p := filepath.Join(fx.home, rel)
		write(p, fx.marker, 0o600)
		fx.homeFiles = append(fx.homeFiles, p)
	}
	write(fx.sock, fx.marker, 0o600)
	write(filepath.Join(fx.other, "f.txt"), fx.marker, 0o600)
	write(filepath.Join(fx.ro, "r.txt"), "ro", 0o600)
	write(filepath.Join(fx.work, "tools", "agent"), "original", 0o700)
	if err := os.MkdirAll(fx.run, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fx.exe = filepath.Join(granted, "bin", "probe")
	if err := os.MkdirAll(filepath.Dir(fx.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := linkOrCopy(self, fx.exe); err != nil {
		t.Fatal(err)
	}
	fx.host = contract.Host{Home: fx.home, Secrets: []string{fx.sock}}
	return fx
}

// linkOrCopy は、src を dst に hard link する (別のファイルシステムなら、コピーする)。
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// echoServer は、path で待ち受け、1 行を読んで "<tag>:<行>" を返す Unix ドメインソケットのサーバ (egress に見立てたもの)。
func echoServer(t *testing.T, path, tag string) net.Listener {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "%s:%s", tag, line)
			}()
		}
	}()
	return l
}

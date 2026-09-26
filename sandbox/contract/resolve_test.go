package contract

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
)

// fakeHome は、ホストの HOME に見立てた一時ディレクトリに、資格情報の偽物を作る。
func fakeHome(t *testing.T) (home, sock string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(dir, "home")
	for _, rel := range []string{".ssh/id", ".claude/c.json", ".gnupg/k", "notes.txt", ".private/kube/config"} {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sock = filepath.Join(dir, "agent.sock")
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, sock
}

// TestResolveRejectsSymlinks は、機密の path を指す symlink を、機密でない名前で見せようとしても、Resolve が断ることを確認する
// (Validate は文字列だけを見るので通す。symlink を辿った実際の path の検証は、Resolve の役目)。
func TestResolveRejectsSymlinks(t *testing.T) {
	home, sock := fakeHome(t)
	links := t.TempDir()
	link := func(name, target string) string {
		p := filepath.Join(links, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	homeTree := filepath.Join(t.TempDir(), "home-via-link")
	if err := os.Symlink(home, homeTree); err != nil {
		t.Fatal(err)
	}
	// ~/.kube 自体が symlink で、実体は HOME の中の別の場所 (InHome を付けても、機密の実体は見せられない)。
	if err := os.Symlink(".private/kube", filepath.Join(home, ".kube")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		host   Host
		src    string
		inHome bool
		want   string
	}{
		{"~/.ssh への symlink", Host{Home: home}, link("ssh", filepath.Join(home, ".ssh")), false, "機密"},
		{"~/.claude への symlink", Host{Home: home}, link("claude", filepath.Join(home, ".claude")), false, "機密"},
		{"HOME 自体への symlink", Host{Home: home}, link("home", home), false, "機密"},
		{"HOME の下 (機密でない) への symlink。InHome なし", Host{Home: home}, link("notes", filepath.Join(home, "notes.txt")), false, "InHome"},
		{"symlink の HOME の下の実体。InHome なし", Host{Home: homeTree}, filepath.Join(home, "notes.txt"), false, "InHome"},
		{"認証用 socket への symlink", Host{Home: home, Secrets: []string{sock}}, link("agent", sock), false, "機密"},
		{"認証用 socket の側が symlink", Host{Home: home, Secrets: []string{link("sockalias", sock)}}, sock, false, "機密"},
		{"/ への symlink", Host{Home: home}, link("root", "/"), false, "機密"},
		{"~/.kube 自体が symlink。その先を直接指定", Host{Home: home}, filepath.Join(home, ".private/kube"), true, "実体"},
		{"symlink の HOME を通して ~/.ssh", Host{Home: homeTree}, filepath.Join(home, ".ssh"), false, "機密"},
		{"存在しない HostPath", Host{Home: home}, filepath.Join(links, "none"), false, "解決できない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Rules{Host: tc.host, Policy: LinuxPolicy(), Caps: sandbox.Capabilities{PathRemap: true}}
			s := sandbox.Spec{Exec: "/bin/true", Read: []sandbox.Mount{{HostPath: tc.src, GuestPath: "/x", InHome: tc.inHome}}}
			_, err := r.Resolve(s)
			if err == nil {
				t.Fatal("Resolve が error にならない")
			}
			if !errors.Is(err, sandbox.ErrRejected) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want ErrRejected で %q を含む", err, tc.want)
			}
			// Write でも同じ。
			s = sandbox.Spec{Exec: "/bin/true", Write: []sandbox.Mount{{HostPath: tc.src, GuestPath: "/x", InHome: tc.inHome}}}
			if _, err := r.Resolve(s); err == nil {
				t.Error("Write の Mount で、Resolve が error にならない")
			}
		})
	}
	// Validate は文字列だけを見るので、機密を指す symlink を通す (Resolve が断る、役割の分担)。
	r := Rules{Host: Host{Home: home}, Policy: LinuxPolicy(), Caps: sandbox.Capabilities{PathRemap: true}}
	s := sandbox.Spec{Exec: "/bin/true", Read: []sandbox.Mount{{HostPath: link("ssh2", filepath.Join(home, ".ssh")), GuestPath: "/x"}}}
	if err := r.Validate(s); err != nil {
		t.Errorf("Validate が、symlink の HostPath を断った (辿るのは Resolve の役目): %v", err)
	}
}

// TestResolveReturnsRealPaths は、Resolve が、symlink を辿った実際の path を、Spec と同じ順で返し、Spec を書き換えないことを確認する。
func TestResolveReturnsRealPaths(t *testing.T) {
	home, _ := fakeHome(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	r := Rules{Host: Host{Home: home}, Policy: LinuxPolicy(), Caps: sandbox.Capabilities{PathRemap: true}}
	s := sandbox.Spec{Exec: "/bin/true", Read: []sandbox.Mount{{HostPath: alias, GuestPath: "/a"}, {HostPath: "/usr", GuestPath: "/usr"}}, Write: []sandbox.Mount{{HostPath: alias, GuestPath: "/w"}}}
	got, err := r.Resolve(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Read) != 2 || got.Read[0] != realDir || got.Read[1] != "/usr" || len(got.Write) != 1 || got.Write[0] != realDir {
		t.Errorf("Resolve = %+v, want Read [%s /usr]・Write [%s]", got, realDir, realDir)
	}
	if s.Read[0].HostPath != alias {
		t.Error("Resolve が、元の Spec を書き換えた")
	}
}

// TestResolveProtectedTreeSymlink は、拒否する path (Policy の保護する path) 自体が symlink のとき、その先も拒否することを確認する
// (例: /etc/ssh が /etc/.ro/ssh への symlink)。Policy に、一時的に symlink を足して試す。
func TestResolveProtectedTreeSymlink(t *testing.T) {
	home, _ := fakeHome(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, alias := filepath.Join(dir, "real"), filepath.Join(dir, "alias")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	pol := LinuxPolicy()
	pol.ProtectedTrees = append(pol.ProtectedTrees, alias)
	r := Rules{Host: Host{Home: home}, Policy: pol, Caps: sandbox.Capabilities{PathRemap: true}}
	s := sandbox.Spec{Exec: "/bin/true", Read: []sandbox.Mount{{HostPath: real, GuestPath: "/x"}}}
	if err := r.Validate(s); err != nil {
		t.Fatalf("Validate (字面だけ) が、実体を断った: %v", err)
	}
	_, err = r.Resolve(s)
	if err == nil || !strings.Contains(err.Error(), "実体") {
		t.Errorf("Resolve = %v, want 保護する path の実体 (symlink の先) の理由", err)
	}
}

package bwrap

import (
	"strings"
	"testing"
)

// bindAt は、cageSpec に、Dst が dst の bind を 1 つ足す。
func bindAt(dst string) func(*Spec) {
	return func(s *Spec) { s.Binds = append(s.Binds, Bind{Src: "/usr/share", Dst: dst}) }
}

// TestArgvRejectsDstBelowSymlinkMore は、dst_symlink_test.go の 4 件に足して、symlink の下の規則の境界を確認する:
// 何番目の symlink でも、深い下でも、symlink の下の symlink でも (どちらの順でも) 拒否する。理由は、symlink の下であること。
func TestArgvRejectsDstBelowSymlinkMore(t *testing.T) {
	cases := []specCase{
		{"最後の symlink (/sbin) の下", bindAt("/sbin/x"), "の下"},
		{"最初の symlink (/lib) の下", bindAt("/lib/x"), "の下"},
		{"深い下", func(s *Spec) {
			s.Symlinks = append(s.Symlinks, Symlink{Target: "/proc", Dst: "/p"})
			s.Binds = append(s.Binds, Bind{Src: "/usr/share", Dst: "/p/a/b/c"})
		}, "の下"},
		{"tmpfs が symlink の下 (既存の symlink)", func(s *Spec) { s.Tmpfs = append(s.Tmpfs, "/bin/x") }, "の下"},
		{"symlink が symlink の下 (上が先)", func(s *Spec) {
			s.Symlinks = append(s.Symlinks, Symlink{Target: "/proc", Dst: "/p"}, Symlink{Target: "x", Dst: "/p/q"})
		}, "の下"},
		{"symlink が symlink の下 (下が先)", func(s *Spec) {
			s.Symlinks = append(s.Symlinks, Symlink{Target: "x", Dst: "/p/q"}, Symlink{Target: "/proc", Dst: "/p"})
		}, "の下"},
		{"symlink と同じ Dst は、重複の規則", bindAt("/bin"), "重複"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { expectRejected(t, c) })
	}
}

// TestArgvErrorNamesSymlink は、symlink の下の Dst の error が、どの要素がどの symlink の下かを示すことを確認する。
func TestArgvErrorNamesSymlink(t *testing.T) {
	s := cageSpec()
	s.Binds = append(s.Binds, Bind{Src: "/usr/share", Dst: "/bin/x"})
	_, err := s.Argv()
	if err == nil {
		t.Fatal("error にならない")
	}
	for _, want := range []string{"Binds[7]", `"/bin/x"`, "Symlinks[2]", `"/bin"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q。%q を含むはず", err, want)
		}
	}
}

// TestArgvAcceptsDstNextToSymlink は、symlink の Dst に、名前が似ているだけの Dst (path の要素の単位で、下ではない) を
// 拒否しすぎないことを確認する。
func TestArgvAcceptsDstNextToSymlink(t *testing.T) {
	s := cageSpec()
	s.Symlinks = append(s.Symlinks, Symlink{Target: "/usr/share", Dst: "/p"})
	s.Binds = append(s.Binds, Bind{Src: "/usr/share", Dst: "/pp"}, Bind{Src: "/usr/share", Dst: "/p2/x"})
	s.Tmpfs = append(s.Tmpfs, "/p-x")
	if _, err := s.Argv(); err != nil {
		t.Errorf("symlink /p に名前が似ているだけの Dst が error: %v", err)
	}
}

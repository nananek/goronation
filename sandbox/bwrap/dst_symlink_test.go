package bwrap

import "testing"

// TestArgvRejectsDstBelowSymlink は、Symlinks の Dst の下に、bind・tmpfs を置けないことを確認する。
// doc.go は「Dst は、/・/proc・/dev の中は使えない」と言うが、Dst の字面だけを見ると、/proc を指す symlink (例: /p) を作り、
// その下 (/p/sys) に bind すると、bwrap は symlink を辿って、檻の /proc/sys の上に mount する
// (/d -> /dev に対する /d/null で、檻の /dev/null を通常ファイルに差し替えることもできた。実 bwrap で確認済み)。
func TestArgvRejectsDstBelowSymlink(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*Spec)
	}{
		{"/proc を指す symlink の下に bind", func(s *Spec) {
			s.Symlinks = append(s.Symlinks, Symlink{Target: "/proc", Dst: "/p"})
			s.Binds = append(s.Binds, Bind{Src: "/usr/share", Dst: "/p/sys"})
		}},
		{"/dev を指す symlink の下に bind (/dev/null の差し替え)", func(s *Spec) {
			s.Symlinks = append(s.Symlinks, Symlink{Target: "/dev", Dst: "/d"})
			s.Binds = append(s.Binds, Bind{Src: "/usr/share/zoneinfo/UTC", Dst: "/d/null"})
		}},
		{"/proc を指す symlink の下に tmpfs", func(s *Spec) {
			s.Symlinks = append(s.Symlinks, Symlink{Target: "/proc", Dst: "/p"})
			s.Tmpfs = append(s.Tmpfs, "/p/sys")
		}},
		{"相対 symlink (../proc) の下に bind", func(s *Spec) {
			s.Symlinks = append(s.Symlinks, Symlink{Target: "../proc", Dst: "/x/p"})
			s.Binds = append(s.Binds, Bind{Src: "/usr/share", Dst: "/x/p/sys"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := cageSpec()
			c.mut(&s)
			if _, err := s.Argv(); err == nil {
				t.Fatal("error にならない (Dst の規則を、symlink 経由で迂回できる)")
			}
		})
	}
}

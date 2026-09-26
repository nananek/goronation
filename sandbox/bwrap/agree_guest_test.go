package bwrap

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
)

// このファイルは、契約の検証と bwrap の検証を、檻の中の path (GuestPath・Scratch・System) についても比べる。
// 契約の検証が、bwrap の検証より弱いと、契約の検証だけに頼る別のバックエンドが、通してはいけない Spec を通す。
// 契約が断って bwrap が通すのは、契約の追加の規則だけ: Scratch (tmpfs) を、Mount・基盤の bind の内側に置く (bwrap は、tmpfs を先に作るので、後から bind される Mount が隠す)。

// scratchHidden は、s の Scratch が、Mount か基盤 (/usr) の bind の内側にあるか (契約の追加の規則が断る配置)。
func scratchHidden(s sandbox.Spec) bool {
	var outer []string
	for _, m := range append(append([]sandbox.Mount{}, s.Read...), s.Write...) {
		outer = append(outer, m.Guest())
	}
	if s.System {
		outer = append(outer, "/usr")
	}
	for _, p := range s.Scratch {
		for _, g := range outer {
			if p != g && strings.HasPrefix(p, g+"/") {
				return true
			}
		}
	}
	return false
}

// agreeGuest は、s について、契約の検証と bwrap の検証の判定が合うことを確かめる。契約だけが断るのは、scratchHidden のときだけ。
func agreeGuest(t *testing.T, b *Backend, s sandbox.Spec, what string) bool {
	t.Helper()
	_, berr := b.translate(s).Argv()
	cerr := b.rules.Validate(s)
	switch {
	case (berr == nil) == (cerr == nil):
		return true
	case berr == nil && cerr != nil && scratchHidden(s):
		return true
	}
	t.Errorf("%s: bwrap = %v, contract = %v\n  spec = %+v", what, berr, cerr, s)
	return false
}

// TestContractAgreesWithBwrapGuestPaths は、檻の中の path (Read・Write の GuestPath と Scratch) と System を動かして、両方の判定が合うことを、表で確認する。
func TestContractAgreesWithBwrapGuestPaths(t *testing.T) {
	b := New(adapterHost)
	guests := []string{
		"/x", "/", "/proc", "/proc/sys", "/procx", "/dev", "/dev/shm", "/devx", "/usr", "/usr/local/bin/goro", "/usr2", "/lib", "/lib/x", "/lib64", "/lib64/y", "/lib32",
		"/bin", "/bin/sh", "/sbin", "/sbin/z", "/tmp", "/tmp/x", "/etc/ssl/certs", "/opt/x", "/work", "/work/a", "rel", "", "/a/../b", "/a\nb", "/x/", "//x",
	}
	n := 0
	for _, g := range guests {
		for _, system := range []bool{false, true} {
			for _, kind := range []string{"read", "write", "scratch"} {
				s := sandbox.Spec{Exec: "/opt/x/x", System: system}
				m := sandbox.Mount{HostPath: "/data/x", GuestPath: g}
				switch kind {
				case "read":
					s.Read = []sandbox.Mount{m}
				case "write":
					s.Write = []sandbox.Mount{m}
				default:
					s.Scratch = []string{g}
				}
				agreeGuest(t, b, s, fmt.Sprintf("guest %q system=%v %s", g, system, kind))
				n++
			}
		}
	}
	// 2 つの path の組 (重複・入れ子・順序)。
	pairs := []string{"/x", "/x/y", "/work", "/work/scr", "/usr", "/usr/x", "/lib", "/lib/x", "/tmp", "/tmp/x", "/proc/x", "/a", "/a/b", "/ab"}
	for _, g1 := range pairs {
		for _, g2 := range pairs {
			for _, system := range []bool{false, true} {
				for _, k1 := range []string{"read", "write", "scratch"} {
					for _, k2 := range []string{"read", "write", "scratch"} {
						s := sandbox.Spec{Exec: "/opt/x/x", System: system}
						add := func(kind, g string, i int) {
							m := sandbox.Mount{HostPath: fmt.Sprintf("/data/h%d", i), GuestPath: g}
							switch kind {
							case "read":
								s.Read = append(s.Read, m)
							case "write":
								s.Write = append(s.Write, m)
							default:
								s.Scratch = append(s.Scratch, g)
							}
						}
						add(k1, g1, 1)
						add(k2, g2, 2)
						agreeGuest(t, b, s, fmt.Sprintf("組 %q(%s) %q(%s) system=%v", g1, k1, g2, k2, system))
						n++
					}
				}
			}
		}
	}
	if n < 1000 {
		t.Errorf("比べた数 = %d, want 1000 以上 (表が空振りしていないか)", n)
	}
}

// TestContractAgreesWithBwrapRandom は、乱数 (固定の seed) で作った Spec (path の部品・Mount 0〜4 個・Scratch 0〜2 個・System・Dir) について、
// 両方の判定が合うことを確認する。レビューで、40 万件の差分 fuzz が、契約だけが通す入力 14,572 件 (/proc・/dev・System の配置) を見つけた。
func TestContractAgreesWithBwrapRandom(t *testing.T) {
	b := New(adapterHost)
	rng := rand.New(rand.NewSource(20260927))
	parts := []string{"proc", "dev", "usr", "lib", "lib64", "lib32", "bin", "sbin", "tmp", "work", "a", "b", "x", "etc", "opt"}
	path := func() string {
		if rng.Intn(40) == 0 {
			return []string{"/", "", "rel", "/a/../b", "/x/", "//x", "/a\nb"}[rng.Intn(7)]
		}
		var sb strings.Builder
		for i, depth := 0, 1+rng.Intn(3); i < depth; i++ {
			sb.WriteString("/" + parts[rng.Intn(len(parts))])
		}
		return sb.String()
	}
	mismatches := 0
	const N = 30000
	for i := 0; i < N && mismatches < 5; i++ {
		s := sandbox.Spec{Exec: "/opt/x/x", System: rng.Intn(2) == 0, Dir: []string{"", "/work", "rel", "/a/../b", "/proc"}[rng.Intn(5)]}
		for j, nm := 0, rng.Intn(5); j < nm; j++ {
			m := sandbox.Mount{HostPath: fmt.Sprintf("/data/h%d", j), GuestPath: path()}
			if rng.Intn(2) == 0 {
				s.Read = append(s.Read, m)
			} else {
				s.Write = append(s.Write, m)
			}
		}
		for j, ns := 0, rng.Intn(3); j < ns; j++ {
			s.Scratch = append(s.Scratch, path())
		}
		if !agreeGuest(t, b, s, fmt.Sprintf("乱数 %d", i)) {
			mismatches++
		}
	}
}

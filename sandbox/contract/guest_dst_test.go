package contract

import (
	"errors"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
)

// TestValidateRejectsGuestInsideProcDev は、檻の中の path (GuestPath・Scratch) が /proc・/dev (とその中) のとき、契約の共通検証が断ることを確認する。
// bwrap は /proc と /dev を常に作るので、その中への mount は、それを差し替える。bwrap の検証 (checkDst) は断るが、契約の共通検証は断らず、
// bwrap の検証だけが門になっている。契約の共通検証だけに頼るバックエンド (PathRemap を宣言する別のバックエンド) は、この Spec を通してしまう。
func TestValidateRejectsGuestInsideProcDev(t *testing.T) {
	m := func(guest string) sandbox.Mount { return sandbox.Mount{HostPath: "/data/x", GuestPath: guest} }
	for name, mutate := range map[string]func(*sandbox.Spec){
		"Read の GuestPath が /proc":    func(s *sandbox.Spec) { s.Read = append(s.Read, m("/proc")) },
		"Read の GuestPath が /proc の中": func(s *sandbox.Spec) { s.Read = append(s.Read, m("/proc/sys")) },
		"Write の GuestPath が /dev":    func(s *sandbox.Spec) { s.Write = append(s.Write, m("/dev")) },
		"Write の GuestPath が /dev の中": func(s *sandbox.Spec) { s.Write = append(s.Write, m("/dev/shm")) },
		"Scratch が /proc の中":          func(s *sandbox.Spec) { s.Scratch = append(s.Scratch, "/proc/self/x") },
		"Scratch が /dev の中":           func(s *sandbox.Spec) { s.Scratch = append(s.Scratch, "/dev/pts") },
	} {
		s := okSpec()
		mutate(&s)
		if err := testRules().Validate(s); !errors.Is(err, sandbox.ErrRejected) {
			t.Errorf("%s: Validate = %v, want ErrRejected", name, err)
		}
	}
}

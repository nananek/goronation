package sandbox

import (
	"errors"
	"fmt"
	"testing"
)

// TestMountGuest は、GuestPath が空なら HostPath と同じで、あればそれを返すことを確認する。
func TestMountGuest(t *testing.T) {
	if got := (Mount{HostPath: "/h"}).Guest(); got != "/h" {
		t.Errorf("GuestPath なし: %q, want /h", got)
	}
	if got := (Mount{HostPath: "/h", GuestPath: "/g"}).Guest(); got != "/g" {
		t.Errorf("GuestPath あり: %q, want /g", got)
	}
}

// TestCapabilitiesHas は、項目の名前から宣言を引けること、未知の名前は false なことを確認する。
func TestCapabilitiesHas(t *testing.T) {
	c := Capabilities{PathRemap: true, KillsDescendants: true}
	for name, want := range map[Cap]bool{CapPathRemap: true, CapKillsDescendants: true, CapPrivateLoopback: false, CapPrivatePIDs: false, "unknown": false, "": false} {
		if got := c.Has(name); got != want {
			t.Errorf("Has(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestExitError は、*ExitError が、errors.As で取り出せ、終了コードを文言に含むことを確認する。
func TestExitError(t *testing.T) {
	var err error = fmt.Errorf("wait: %w", &ExitError{Code: 137})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 137 {
		t.Fatalf("errors.As = %v, %+v", errors.As(err, &ee), ee)
	}
	if got := ee.Error(); got == "" || !contains(got, "137") {
		t.Errorf("Error() = %q, want 137 を含む", got)
	}
}

// TestErrorsAreDistinct は、契約の error が、互いに別物なことを確認する (呼び手が errors.Is で判別できる)。
func TestErrorsAreDistinct(t *testing.T) {
	all := []error{ErrNotInstalled, ErrUnusable, ErrRejected, ErrTerminalUnsafe}
	for i, a := range all {
		for j, b := range all {
			if (i == j) != errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = %v", a, b, errors.Is(a, b))
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

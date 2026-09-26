package contract

import "testing"

// TestPathHelpers は、path の検査と、包含の判定を確認する。
func TestPathHelpers(t *testing.T) {
	for _, p := range []string{"/", "/a", "/a/b", "/a b", "/名前"} {
		if err := CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"", "a", "a/b", "./a", "/a/", "/a//b", "/a/../b", "/a/./b", "/a\nb", "/a\x00b", "/a\x7fb"} {
		if err := CheckPath(p); err == nil {
			t.Errorf("CheckPath(%q) が error にならない", p)
		}
	}
	for _, tc := range []struct {
		p, root string
		want    bool
	}{
		{"/a", "/a", true}, {"/a/b", "/a", true}, {"/ab", "/a", false}, {"/a", "/a/b", false}, {"/x", "/", true}, {"/", "/", true}, {"/", "/a", false},
	} {
		if got := Under(tc.p, tc.root); got != tc.want {
			t.Errorf("Under(%q, %q) = %v, want %v", tc.p, tc.root, got, tc.want)
		}
	}
	if !Overlap("/a", "/a/b") || !Overlap("/a/b", "/a") || Overlap("/a", "/ab") {
		t.Error("Overlap の判定が違う")
	}
}

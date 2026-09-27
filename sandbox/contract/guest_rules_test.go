package contract

import (
	"errors"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
)

// guestSpec は、基盤 (System) と、guest に置いた Mount 1 つを持つ Spec。
func guestSpec(system bool, guest string, write bool) sandbox.Spec {
	s := sandbox.Spec{Exec: "/opt/x/x", System: system}
	m := sandbox.Mount{HostPath: "/data/x", GuestPath: guest}
	if write {
		s.Write = []sandbox.Mount{m}
	} else {
		s.Read = []sandbox.Mount{m}
	}
	return s
}

// TestValidateSystemReservedPaths は、System が占める path (/usr の bind と、/lib・/lib64・/bin・/sbin の symlink) に、Mount と Scratch を置けないことを確認する
// (/usr の中は置ける。symlink は、その path 自体にも、中にも置けない)。System でなければ、置ける。
func TestValidateSystemReservedPaths(t *testing.T) {
	r := testRules()
	for guest, want := range map[string]string{
		"/usr": "重複", "/lib": "重複", "/lib64": "重複", "/bin": "重複", "/sbin": "重複",
		"/lib/x": "symlink", "/lib64/y/z": "symlink", "/bin/sh": "symlink", "/sbin/x": "symlink",
	} {
		for _, write := range []bool{false, true} {
			err := r.Validate(guestSpec(true, guest, write))
			if !errors.Is(err, sandbox.ErrRejected) || !strings.Contains(err.Error(), want) {
				t.Errorf("System で GuestPath %s (write=%v): %v, want ErrRejected で %q を含む", guest, write, err, want)
			}
		}
		s := guestSpec(true, "/x", false)
		s.Read, s.Scratch = nil, []string{guest}
		if err := r.Validate(s); !errors.Is(err, sandbox.ErrRejected) {
			t.Errorf("System で Scratch %s: %v, want ErrRejected", guest, err)
		}
	}
	for _, guest := range []string{"/usr/local/bin/goro", "/usr/x", "/libx", "/lib32", "/binx", "/usr2", "/opt/x"} {
		if err := r.Validate(guestSpec(true, guest, false)); err != nil {
			t.Errorf("System で GuestPath %s: 断った: %v", guest, err)
		}
	}
	for _, guest := range []string{"/usr", "/lib", "/lib/x", "/bin/sh"} {
		if err := r.Validate(guestSpec(false, guest, false)); err != nil {
			t.Errorf("System でないとき GuestPath %s: 断った: %v (基盤が占めない)", guest, err)
		}
	}
}

// TestValidateReservedGuestPaths は、バックエンドが常に作る path (Linux は /proc・/dev) の中に、Mount と Scratch を置けないことを確認する。
// 名前が似ているだけの path (/procx・/devices) は置ける。表 (Policy.GuestReserved) が空なら、断らない (OS ごとの表)。
func TestValidateReservedGuestPaths(t *testing.T) {
	r := testRules()
	for _, guest := range []string{"/proc", "/proc/sys", "/proc/self/x", "/dev", "/dev/shm", "/dev/pts/0"} {
		for _, write := range []bool{false, true} {
			if err := r.Validate(guestSpec(false, guest, write)); !errors.Is(err, sandbox.ErrRejected) || !strings.Contains(err.Error(), "常に作る") {
				t.Errorf("GuestPath %s (write=%v): %v, want ErrRejected", guest, write, err)
			}
		}
		s := guestSpec(false, "/x", false)
		s.Read, s.Scratch = nil, []string{guest}
		if err := r.Validate(s); !errors.Is(err, sandbox.ErrRejected) {
			t.Errorf("Scratch %s: %v, want ErrRejected", guest, err)
		}
	}
	for _, guest := range []string{"/procx", "/devices", "/dev2/x", "/x/proc", "/x/dev"} {
		if err := r.Validate(guestSpec(false, guest, false)); err != nil {
			t.Errorf("GuestPath %s: 断った: %v", guest, err)
		}
	}
	r.Policy.GuestReserved = nil
	if err := r.Validate(guestSpec(false, "/proc/x", false)); err != nil {
		t.Errorf("表が空なのに断った: %v", err)
	}
}

// TestValidateScratchInsideMount は、Scratch (tmpfs) を、Mount・基盤の bind の内側に置けないことを確認する (tmpfs は Mount より先に作られるので、
// 後から bind される Mount が、Scratch を隠す。rw なら、書いたものがホストに残る)。Mount の外側の Scratch は置ける。
func TestValidateScratchInsideMount(t *testing.T) {
	r := testRules()
	spec := func(scratch string, read, write []sandbox.Mount, system bool) sandbox.Spec {
		return sandbox.Spec{Exec: "/opt/x/x", System: system, Scratch: []string{scratch}, Read: read, Write: write}
	}
	m := func(guest string) sandbox.Mount { return sandbox.Mount{HostPath: "/data/x", GuestPath: guest} }
	for name, s := range map[string]sandbox.Spec{
		"Write の中":       spec("/work/scr", nil, []sandbox.Mount{m("/work")}, false),
		"Read の中":        spec("/ro/scr", []sandbox.Mount{m("/ro")}, nil, false),
		"深い中":            spec("/work/a/b/c", nil, []sandbox.Mount{m("/work")}, false),
		"基盤 (/usr) の中":   spec("/usr/scr", nil, nil, true),
		"基盤 (/usr) の深い中": spec("/usr/local/scr", nil, nil, true),
	} {
		err := r.Validate(s)
		if !errors.Is(err, sandbox.ErrRejected) || !strings.Contains(err.Error(), "内側") && !strings.Contains(err.Error(), "symlink") {
			t.Errorf("Scratch が Mount の内側 (%s): %v, want ErrRejected", name, err)
		}
	}
	for name, s := range map[string]sandbox.Spec{
		"Mount の外側":      spec("/a", []sandbox.Mount{m("/a/b")}, nil, false),
		"Mount と別の場所":    spec("/tmp", nil, []sandbox.Mount{m("/work")}, false),
		"名前が似ているだけ":      spec("/work2", nil, []sandbox.Mount{m("/work")}, false),
		"基盤 (/usr) でない":  spec("/usr", nil, nil, false),
		"System で /usrx": spec("/usrx", nil, nil, true),
	} {
		if err := r.Validate(s); err != nil {
			t.Errorf("Scratch (%s): 断った: %v", name, err)
		}
	}
	// Scratch の中の Scratch は、どちらも tmpfs (ホストに残らない) なので、置ける。
	s := sandbox.Spec{Exec: "/opt/x/x", Scratch: []string{"/tmp", "/tmp/inner"}}
	if err := r.Validate(s); err != nil {
		t.Errorf("Scratch の中の Scratch: %v", err)
	}
}

// TestValidateExtraEnvIsChecked は、バックエンドが宣言する ExtraEnv の名前も、環境変数の規則に通すことを確認する
// (宣言で、資格情報らしい名前を、檻に足させない。検証は、Spec の中身に依らず断る)。
func TestValidateExtraEnvIsChecked(t *testing.T) {
	r := testRules()
	for _, name := range []string{"PWD", "TMPDIR", "XDG_RUNTIME_DIR_X"} {
		r.Caps.ExtraEnv = []string{name}
		if err := r.Validate(okSpec()); err != nil {
			t.Errorf("ExtraEnv %q: 断った: %v", name, err)
		}
	}
	for _, name := range []string{"GH_TOKEN", "SSH_AUTH_SOCK", "AWS_REGION", "MY_SECRET", "", "A=B", "A B", "1A"} {
		r.Caps.ExtraEnv = []string{"PWD", name}
		err := r.Validate(okSpec())
		if !errors.Is(err, sandbox.ErrRejected) || !strings.Contains(err.Error(), "ExtraEnv[1]") {
			t.Errorf("ExtraEnv %q: %v, want ErrRejected (ExtraEnv[1])", name, err)
		}
	}
}

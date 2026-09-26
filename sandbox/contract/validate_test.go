package contract

import (
	"errors"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/sandbox"
)

// testRules は、ホストの HOME が /home/tester で、認証用 socket が /run/user/1000/agent.sock の、Linux の規則 (path を付け替えられる)。
func testRules() Rules {
	return Rules{
		Host:   Host{Home: "/home/tester", Secrets: []string{"/run/user/1000/agent.sock"}},
		Policy: LinuxPolicy(),
		Caps:   sandbox.Capabilities{PathRemap: true},
	}
}

// okSpec は、検証に通る Spec (基盤・実行ファイル・作業ディレクトリ・egress の UDS の親)。
func okSpec() sandbox.Spec {
	return sandbox.Spec{
		Exec: "/opt/probe/probe", Args: []string{"init"}, Dir: "/work", System: true, Scratch: []string{"/tmp"},
		Env:      []sandbox.EnvVar{{Key: "HOME", Value: "/work"}, {Key: "PATH", Value: "/usr/bin:/bin"}},
		Read:     []sandbox.Mount{{HostPath: "/opt/agent/bin", GuestPath: "/opt/probe/probe"}, {HostPath: "/data/run", GuestPath: "/run/goro"}},
		Write:    []sandbox.Mount{{HostPath: "/data/clone", GuestPath: "/work"}},
		Egress:   "/run/goro/proxy.sock",
		Loopback: []string{"127.0.0.1:3128"},
	}
}

// TestValidateAccepts は、正しい Spec と、契約が許す形を、通すことを確認する。
func TestValidateAccepts(t *testing.T) {
	r := testRules()
	if err := r.Validate(okSpec()); err != nil {
		t.Fatalf("正しい Spec を断った: %v", err)
	}
	for name, mutate := range map[string]func(*sandbox.Spec){
		"HOME の下の作業用 dir (InHome あり)": func(s *sandbox.Spec) {
			s.Write = append(s.Write, sandbox.Mount{HostPath: "/home/tester/work/x", GuestPath: "/x", InHome: true})
		},
		"HOME の下の .opencode/bin (機密でない)": func(s *sandbox.Spec) {
			s.Read = append(s.Read, sandbox.Mount{HostPath: "/home/tester/.opencode/bin/opencode", GuestPath: "/x", InHome: true})
		},
		"名前が機密に似ているだけの path (.sshx)": func(s *sandbox.Spec) {
			s.Read = append(s.Read, sandbox.Mount{HostPath: "/home/tester/.sshx", GuestPath: "/x", InHome: true})
		},
		"/tmp の中の path":          func(s *sandbox.Spec) { s.Read = append(s.Read, sandbox.Mount{HostPath: "/tmp/x", GuestPath: "/x"}) },
		"GuestPath を省略 (ホストと同じ)": func(s *sandbox.Spec) { s.Read = append(s.Read, sandbox.Mount{HostPath: "/data/y"}) },
		"Egress も Scratch も無い":   func(s *sandbox.Spec) { s.Egress, s.Scratch = "", nil },
		"loopback の待ち受け (IPv6)":  func(s *sandbox.Spec) { s.Loopback = append(s.Loopback, "[::1]:8080") },
	} {
		s := okSpec()
		mutate(&s)
		if err := r.Validate(s); err != nil {
			t.Errorf("%s: 断った: %v", name, err)
		}
	}
}

// TestValidateRejects は、契約に反する Spec を、ErrRejected で断ることを確認する (文字列だけを見る)。
func TestValidateRejects(t *testing.T) {
	r := testRules()
	read := func(m sandbox.Mount) func(*sandbox.Spec) {
		return func(s *sandbox.Spec) { s.Read = append(s.Read, m) }
	}
	write := func(m sandbox.Mount) func(*sandbox.Spec) {
		return func(s *sandbox.Spec) { s.Write = append(s.Write, m) }
	}
	for name, tc := range map[string]struct {
		mutate func(*sandbox.Spec)
		want   string // error に含まれるはずの語
	}{
		"HOME 自体":                 {read(sandbox.Mount{HostPath: "/home/tester", GuestPath: "/x", InHome: true}), "HOME"},
		"HOME を含む path (/home)":   {read(sandbox.Mount{HostPath: "/home", GuestPath: "/x"}), "HOME"},
		"/":                       {read(sandbox.Mount{HostPath: "/", GuestPath: "/x"}), "HOME"},
		"~/.ssh":                  {read(sandbox.Mount{HostPath: "/home/tester/.ssh", GuestPath: "/x", InHome: true}), "機密"},
		"~/.ssh の下":               {read(sandbox.Mount{HostPath: "/home/tester/.ssh/id", GuestPath: "/x", InHome: true}), "機密"},
		"~/.claude.json":          {read(sandbox.Mount{HostPath: "/home/tester/.claude.json", GuestPath: "/x", InHome: true}), "機密"},
		"~/.claude-code (接頭辞)":    {read(sandbox.Mount{HostPath: "/home/tester/.claude-code", GuestPath: "/x", InHome: true}), "機密"},
		"~/.gnupg-vault":          {read(sandbox.Mount{HostPath: "/home/tester/.gnupg-vault", GuestPath: "/x", InHome: true}), "機密"},
		"~/.config (機密を含む親)":      {read(sandbox.Mount{HostPath: "/home/tester/.config", GuestPath: "/x", InHome: true}), "機密"},
		"~/.local/share/opencode": {read(sandbox.Mount{HostPath: "/home/tester/.local/share/opencode", GuestPath: "/x", InHome: true}), "機密"},
		"~/.config/opencode":      {read(sandbox.Mount{HostPath: "/home/tester/.config/opencode", GuestPath: "/x", InHome: true}), "機密"},
		"HOME の下で InHome が無い":     {read(sandbox.Mount{HostPath: "/home/tester/work/x", GuestPath: "/x"}), "InHome"},
		"HOME の外で InHome を付けた":    {read(sandbox.Mount{HostPath: "/data/x", GuestPath: "/x", InHome: true}), "InHome"},
		"認証用 socket":              {read(sandbox.Mount{HostPath: "/run/user/1000/agent.sock", GuestPath: "/x"}), "機密"},
		"認証用 socket の親":           {read(sandbox.Mount{HostPath: "/run/user/1000", GuestPath: "/x"}), "機密"},
		"/run":                    {read(sandbox.Mount{HostPath: "/run/x", GuestPath: "/x"}), "機密"},
		"/proc":                   {read(sandbox.Mount{HostPath: "/proc/self", GuestPath: "/x"}), "機密"},
		"/etc/ssh":                {read(sandbox.Mount{HostPath: "/etc/ssh", GuestPath: "/x"}), "機密"},
		"/etc (機密を含む親)":           {read(sandbox.Mount{HostPath: "/etc", GuestPath: "/x"}), "機密"},
		"/tmp 自体 (共有)":            {read(sandbox.Mount{HostPath: "/tmp", GuestPath: "/x"}), "共有"},
		"/usr を Write に":          {write(sandbox.Mount{HostPath: "/usr", GuestPath: "/x"}), "Read でだけ"},
		"/opt/x を Write に":        {write(sandbox.Mount{HostPath: "/opt/x", GuestPath: "/x"}), "Read でだけ"},
		"相対の HostPath":            {read(sandbox.Mount{HostPath: "rel/x", GuestPath: "/x"}), "絶対"},
		"クリーンでない HostPath":        {read(sandbox.Mount{HostPath: "/data/../x", GuestPath: "/x"}), "クリーン"},
		"制御文字を含む HostPath":        {read(sandbox.Mount{HostPath: "/data/a\nb", GuestPath: "/x"}), "制御文字"},
		"空の HostPath":             {read(sandbox.Mount{GuestPath: "/x"}), "空"},
		"相対の GuestPath":           {read(sandbox.Mount{HostPath: "/data/x", GuestPath: "rel"}), "絶対"},
		"GuestPath が /":           {read(sandbox.Mount{HostPath: "/data/x", GuestPath: "/"}), "/"},
		"GuestPath の重複":           {read(sandbox.Mount{HostPath: "/data/x", GuestPath: "/work"}), "重複"},
		"Scratch と Mount の重複":     {read(sandbox.Mount{HostPath: "/data/x", GuestPath: "/tmp"}), "重複"},
		"資格情報らしい環境変数 (GH_TOKEN)": {func(s *sandbox.Spec) { s.Env = append(s.Env, sandbox.EnvVar{Key: "GH_TOKEN", Value: "x"}) }, "資格情報"},
		"SSH_AUTH_SOCK":       {func(s *sandbox.Spec) { s.Env = append(s.Env, sandbox.EnvVar{Key: "SSH_AUTH_SOCK", Value: "/x"}) }, "資格情報"},
		"名前に = を含む環境変数":       {func(s *sandbox.Spec) { s.Env = append(s.Env, sandbox.EnvVar{Key: "A=B", Value: "x"}) }, "名前"},
		"環境変数の値に改行":           {func(s *sandbox.Spec) { s.Env = append(s.Env, sandbox.EnvVar{Key: "A", Value: "x\ny"}) }, "改行"},
		"環境変数の重複":             {func(s *sandbox.Spec) { s.Env = append(s.Env, sandbox.EnvVar{Key: "HOME", Value: "/y"}) }, "重複"},
		"相対の Exec":            {func(s *sandbox.Spec) { s.Exec = "probe" }, "絶対"},
		"空の Exec":             {func(s *sandbox.Spec) { s.Exec = "" }, "空"},
		"NUL を含む引数":           {func(s *sandbox.Spec) { s.Args = append(s.Args, "a\x00b") }, "NUL"},
		"相対の Dir":             {func(s *sandbox.Spec) { s.Dir = "work" }, "絶対"},
		"Egress の親が Read に無い": {func(s *sandbox.Spec) { s.Egress = "/other/p.sock" }, "Read に無い"},
		"Egress の親が Write": {func(s *sandbox.Spec) {
			s.Write = append(s.Write, sandbox.Mount{HostPath: "/data/run2", GuestPath: "/run/goro2"})
			s.Egress = "/run/goro2/p"
		}, "Read に無い"},
		"相対の Egress":           {func(s *sandbox.Spec) { s.Egress = "p.sock" }, "絶対"},
		"loopback でない待ち受け":     {func(s *sandbox.Spec) { s.Loopback = []string{"0.0.0.0:3128"} }, "loopback"},
		"名前 (localhost) の待ち受け": {func(s *sandbox.Spec) { s.Loopback = []string{"localhost:3128"} }, "loopback"},
		"ポートの無い待ち受け":           {func(s *sandbox.Spec) { s.Loopback = []string{"127.0.0.1"} }, "loopback"},
	} {
		s := okSpec()
		tc.mutate(&s)
		err := r.Validate(s)
		if err == nil {
			t.Errorf("%s: 断らない", name)
			continue
		}
		if !errors.Is(err, sandbox.ErrRejected) {
			t.Errorf("%s: error が ErrRejected を包んでいない: %v", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want %q を含む", name, err, tc.want)
		}
	}
}

// TestValidateEgressDirIsReadOnly は、Egress の親ディレクトリを Write で見せると断る (檻が、ソケットを差し替えられる) ことを確認する。
func TestValidateEgressDirIsReadOnly(t *testing.T) {
	s := okSpec()
	s.Read = s.Read[:1]                                                                     // /run/goro を、Read から外す
	s.Write = append(s.Write, sandbox.Mount{HostPath: "/data/run", GuestPath: "/run/goro"}) // Write で見せる
	err := testRules().Validate(s)
	if err == nil || !strings.Contains(err.Error(), "Read に無い") {
		t.Errorf("Egress の親ディレクトリが Write のとき: %v", err)
	}
	s.Read = append(s.Read, sandbox.Mount{HostPath: "/data/run3", GuestPath: "/run/other"})
	s.Egress = "/run/other/p.sock"
	s.Write = append(s.Write, sandbox.Mount{HostPath: "/data/run4", GuestPath: "/run/other2"})
	if err := testRules().Validate(s); err != nil {
		t.Errorf("Egress の親が Read で、別の dir が Write のときは通る: %v", err)
	}
}

// TestValidateHost は、Host が不正なとき (HOME が空・/・相対・Secrets が相対) に、断ることを確認する。
func TestValidateHost(t *testing.T) {
	for name, h := range map[string]Host{
		"HOME が空":     {},
		"HOME が /":    {Home: "/"},
		"HOME が相対":    {Home: "home/x"},
		"Secrets が相対": {Home: "/home/tester", Secrets: []string{"rel"}},
	} {
		r := testRules()
		r.Host = h
		if err := r.Validate(okSpec()); !errors.Is(err, sandbox.ErrRejected) {
			t.Errorf("%s: %v, want ErrRejected", name, err)
		}
	}
}

// TestValidatePathRemapFalse は、path を付け替えられないバックエンド (PathRemap = false。偽のバックエンド) で、GuestPath が HostPath と違う Spec と、
// Scratch を、断ることを確認する。GuestPath の省略と、HostPath と同じ GuestPath (ホストと同じ配置) は、通る。
func TestValidatePathRemapFalse(t *testing.T) {
	r := testRules()
	r.Caps.PathRemap = false
	identity := sandbox.Spec{
		Exec: "/data/agent/bin", System: true,
		Read:  []sandbox.Mount{{HostPath: "/data/agent/bin"}, {HostPath: "/data/run", GuestPath: "/data/run"}},
		Write: []sandbox.Mount{{HostPath: "/data/clone"}},
		Dir:   "/data/clone", Egress: "/data/run/proxy.sock",
	}
	if err := r.Validate(identity); err != nil {
		t.Fatalf("ホストと同じ配置を断った: %v", err)
	}
	if err := testRules().Validate(okSpec()); err != nil {
		t.Fatalf("PathRemap = true の同じ Spec: %v", err)
	}
	if err := r.Validate(okSpec()); !errors.Is(err, sandbox.ErrRejected) || !strings.Contains(err.Error(), "付け替えられない") {
		t.Errorf("付け替えを含む Spec (PathRemap = false): %v, want ErrRejected", err)
	}
	s := identity
	s.Scratch = []string{"/tmp"}
	if err := r.Validate(s); !errors.Is(err, sandbox.ErrRejected) {
		t.Errorf("Scratch (PathRemap = false): %v, want ErrRejected", err)
	}
}

// TestValidateIsPure は、Validate が Spec を書き換えないことを確認する。
func TestValidateIsPure(t *testing.T) {
	s := okSpec()
	before := len(s.Read) + len(s.Write) + len(s.Env)
	_ = testRules().Validate(s)
	if got := len(s.Read) + len(s.Write) + len(s.Env); got != before || s.Exec != "/opt/probe/probe" {
		t.Error("Validate が Spec を書き換えた")
	}
}

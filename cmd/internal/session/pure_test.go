package session

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nananek/goronation/sandbox/bwrap"
)

const oid = "0123456789abcdef0123456789abcdef01234567"

func TestParseBundleHeader(t *testing.T) {
	if maxHeads != 100 {
		t.Fatalf("maxHeads = %d (このテストは、100 を前提にする)", maxHeads)
	}
	long := "refs/heads/" + strings.Repeat("a", 201) // 名前が 200 文字を超える
	var many strings.Builder
	for i := 0; i < 105; i++ {
		many.WriteString(oid + " refs/heads/b" + strconv.Itoa(i) + "\n")
	}
	for _, tc := range []struct {
		name    string
		in      string
		heads   []string
		skipped int
		wantErr bool
	}{
		{"v2 の 1 ブランチ", "# v2 git bundle\n" + oid + " refs/heads/main\n\nPACK", []string{"refs/heads/main"}, 0, false},
		{"v3 と capability・前提・HEAD・タグ", "# v3 git bundle\n@object-format=sha1\n-" + oid + " prereq\n" + oid + " HEAD\n" + oid + " refs/heads/a/b-c_d.e\n" + oid + " refs/tags/v1\n\nPACK",
			[]string{"refs/heads/a/b-c_d.e"}, 0, false},
		{"名前が 200 文字ちょうど", "# v2 git bundle\n" + oid + " refs/heads/" + strings.Repeat("a", 200) + "\n\nPACK", []string{"refs/heads/" + strings.Repeat("a", 200)}, 0, false},
		{"名前が _ で始まる", "# v2 git bundle\n" + oid + " refs/heads/_ok\n" + oid + " refs/heads/a$b\n\nPACK", []string{"refs/heads/_ok"}, 1, false},
		{"sha256 の oid", "# v3 git bundle\n" + strings.Repeat("a", 64) + " refs/heads/x\n\nPACK", []string{"refs/heads/x"}, 0, false},
		{"安全でない名前は除く", "# v2 git bundle\n" + oid + " refs/heads/ok\n" +
			oid + " refs/heads/evil;touch\n" + oid + " refs/heads/$(id)\n" + oid + " refs/heads/a`b`\n" + oid + " refs/heads/a'b\n" +
			oid + " refs/heads/a\"b\n" + oid + " refs/heads/a b\n" + oid + " refs/heads/a|b\n" + oid + " refs/heads/a&b\n" +
			oid + " refs/heads/a>b\n" + oid + " refs/heads/a<b\n" + oid + " refs/heads/a\\b\n" + oid + " refs/heads/日本語\n" +
			oid + " refs/heads/-x\n" + oid + " refs/heads/a..b\n" + oid + " refs/heads/a//b\n" + oid + " refs/heads/a/\n" +
			oid + " refs/heads/x.lock\n" + oid + " refs/heads/a.lock/b\n" + oid + " refs/heads/.hidden\n" + oid + " refs/heads/a/.b\n" +
			oid + " refs/heads/a.\n" + oid + " refs/heads/\n" + oid + " " + long + "\n\nPACK", []string{"refs/heads/ok"}, 23, false},
		{"上限を超えたブランチは除く", "# v2 git bundle\n" + many.String() + "\nPACK", nil, 5, false},
		{"先頭が違う", "# v1 git bundle\n\nPACK", nil, 0, true},
		{"空", "", nil, 0, true},
		{"空行が無い", "# v2 git bundle\n" + oid + " refs/heads/main\n", nil, 0, true},
		{"空行が無く、末尾も改行でない", "# v2 git bundle\n" + oid + " refs/heads/main", nil, 0, true},
		{"行が oid だけ (区切りの空白が無い)", "# v2 git bundle\n" + oid + "\n" + oid + " refs/heads/main\n\nPACK", nil, 0, true},
		{"oid が短い", "# v2 git bundle\nabc refs/heads/main\n\nPACK", nil, 0, true},
		{"oid の前に余分な文字", "# v2 git bundle\nzz" + oid + " refs/heads/main\n\nPACK", nil, 0, true},
		{"oid の後ろに余分な文字", "# v2 git bundle\n" + oid + "zz refs/heads/main\n\nPACK", nil, 0, true},
		{"oid が 41 桁", "# v2 git bundle\n" + oid + "a refs/heads/main\n\nPACK", nil, 0, true},
		{"oid が 63 桁", "# v2 git bundle\n" + strings.Repeat("a", 63) + " refs/heads/main\n\nPACK", nil, 0, true},
		{"oid が 65 桁", "# v2 git bundle\n" + strings.Repeat("a", 65) + " refs/heads/main\n\nPACK", nil, 0, true},
		{"oid が大文字", "# v2 git bundle\n" + strings.ToUpper(oid) + " refs/heads/main\n\nPACK", nil, 0, true},
		{"行に空白が無い", "# v2 git bundle\n" + oid + "refs/heads/main\n\nPACK", nil, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			heads, skipped, err := parseBundleHeader([]byte(tc.in))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if tc.name == "上限を超えたブランチは除く" {
				if len(heads) != 100 {
					t.Errorf("ブランチ数 = %d, want 100 (上限)", len(heads))
				}
			} else if !slices.Equal(heads, tc.heads) {
				t.Errorf("heads = %q, want %q", heads, tc.heads)
			}
			if skipped != tc.skipped {
				t.Errorf("skipped = %d, want %d", skipped, tc.skipped)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	for _, s := range []string{"plain", "a b", "it's", "'", "''", `a\b`, "$(touch x)", "`id`", "a;b", "a\nb", "*", "~", "", "日本語", "-n"} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("shellQuote(%q) を sh に通すと %q (%v), want 元の文字列", s, out, err)
		}
	}
}

func TestFetchCommand(t *testing.T) {
	got := fetchCommand("/x y/it's.bundle", "20260101-000000-abcdef", []string{"refs/heads/main", "refs/heads/feat/x"})
	want := `git -c transfer.fsckObjects=true fetch --no-tags --no-recurse-submodules '/x y/it'\''s.bundle' 'refs/heads/main:refs/heads/goro/20260101-000000-abcdef/main' 'refs/heads/feat/x:refs/heads/goro/20260101-000000-abcdef/feat/x'`
	if got != want {
		t.Errorf("fetchCommand:\n got %s\nwant %s", got, want)
	}
}

func TestIdentity(t *testing.T) {
	if n, e, err := identity("", ""); err != nil || n != DefaultName || e != DefaultEmail {
		t.Errorf("既定: %q %q %v", n, e, err)
	}
	if n, e, err := identity("Alice B", "alice@example.com"); err != nil || n != "Alice B" || e != "alice@example.com" {
		t.Errorf("指定: %q %q %v", n, e, err)
	}
	for _, bad := range [][2]string{
		{"a\nb", "a@b.c"}, {"a\x00b", "a@b.c"}, {" a", "a@b.c"}, {"a ", "a@b.c"}, {strings.Repeat("a", 201), "a@b.c"}, {"a<b", "a@b.c"}, {"a>b", "a@b.c"},
		{"a", "a\n@b.c"}, {"a", "no-at"}, {"a", "a b@c.d"}, {"a", "<a@b.c>"}, {"a", strings.Repeat("a", 200) + "@b.c"}, {"a", "a@b.c\r"}, {"a\x7fb", "a@b.c"}, {"a", "a\x7f@b.c"},
	} {
		if _, _, err := identity(bad[0], bad[1]); err == nil {
			t.Errorf("identity(%q, %q) が error にならない", bad[0], bad[1])
		}
	}
}

func TestCheckRepo(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o644)
	if got, err := checkRepo(dir); err != nil || got != dir {
		t.Errorf("checkRepo(dir) = %q, %v", got, err)
	}
	t.Chdir(dir)
	if got, err := checkRepo("."); err != nil || got != dir {
		t.Errorf("checkRepo(.) = %q, %v, want %q (絶対 path にする)", got, err, dir)
	}
	for _, bad := range []struct{ repo, want string }{
		{"", "空"}, {"https://example.com/r.git", "URL"}, {"ssh://git@example.com/r.git", "URL"}, {"git://example.com/r", "URL"},
		{"file:///tmp/x", "URL"}, {"http://x/y", "URL"}, {"-c", "- で始まる"}, {"--upload-pack=x", "- で始まる"},
		{file, "ディレクトリではない"}, {filepath.Join(dir, "none"), "no such file"}, {dir + "/a\nb", "制御文字"},
	} {
		if got, err := checkRepo(bad.repo); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("checkRepo(%q) = %q, %v, want %q を含む error", bad.repo, got, err, bad.want)
		}
	}
}

func TestSessionIDs(t *testing.T) {
	for i := 0; i < 20; i++ {
		id, err := newID()
		if err != nil || !idRE.MatchString(id) {
			t.Fatalf("newID = %q, %v", id, err)
		}
	}
	for _, ok := range []string{"20260926-103000-a1b2c3", "00000000-000000-000000"} {
		if !idRE.MatchString(ok) {
			t.Errorf("%q を拒否した", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "20260926-103000-a1b2c", "20260926-103000-a1b2c3d", "20260926-103000-A1B2C3", "2026092-103000-a1b2c3",
		"20260926-103000-a1b2c3\n", "x20260926-103000-a1b2c3", "../20260926-103000-a1b2c3", "a/20260926-103000-a1b2c3", "20260926-103000-a1b2c3/..", "/etc", "20260926_103000_a1b2c3"} {
		if idRE.MatchString(bad) {
			t.Errorf("%q を通した", bad)
		}
	}
}

func TestNewStore(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "state"), bwrap.Host{Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dir, "state"), st.root} {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v, want 0700 のディレクトリ", d, fi, err)
		}
	}
	// 既にある、緩いモードのディレクトリも、0700 にする。
	loose := filepath.Join(dir, "loose")
	os.MkdirAll(filepath.Join(loose, "sessions"), 0o755)
	if _, err := NewStore(loose, bwrap.Host{Home: dir}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{loose, filepath.Join(loose, "sessions")} {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s のモード = %v, want 0700", d, fi.Mode().Perm())
		}
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o644)
	for _, bad := range []string{"", "rel/dir", dir + "/a/../b", dir + "/trailing/", dir + "/a\nb", dir + "/a\x7fb", file} {
		if _, err := NewStore(bad, bwrap.Host{Home: dir}); err == nil {
			t.Errorf("NewStore(%q) が error にならない", bad)
		}
	}
}

func TestGet(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "state"), bwrap.Host{Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	id := "20260926-103000-a1b2c3"
	if _, err := st.Get(id); err == nil {
		t.Error("存在しないセッションが、error にならない")
	}
	os.Mkdir(filepath.Join(st.root, id), 0o700)
	s, err := st.Get(id)
	if err != nil || s.ID != id || s.Clone != filepath.Join(st.root, id, "clone") || s.Export != filepath.Join(st.root, id, "export") || s.Run != filepath.Join(st.root, id, "run") {
		t.Errorf("Get = %+v, %v", s, err)
	}
	for _, bad := range []string{"", "..", "../x", "a/b", "x"} {
		if _, err := st.Get(bad); err == nil {
			t.Errorf("Get(%q) が error にならない", bad)
		}
	}
	// ディレクトリでない・symlink は、拒否する。
	other := "20260926-103000-ffffff"
	os.WriteFile(filepath.Join(st.root, other), nil, 0o600)
	if _, err := st.Get(other); err == nil {
		t.Error("ファイルのセッションが、error にならない")
	}
	link := "20260926-103000-eeeeee"
	os.Symlink(dir, filepath.Join(st.root, link))
	if _, err := st.Get(link); err == nil {
		t.Error("symlink のセッションが、error にならない")
	}
}

// wantGitEnv は、檻の中の git の環境変数の、期待する全体 (gitEnv とは別に書く。ホストの環境変数は、1 つも渡さない)。
var wantGitEnv = map[string]string{
	"HOME": "/home/goro", "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8",
	"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_COUNT": "3",
	"GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "*",
	"GIT_CONFIG_KEY_1": "core.fsmonitor", "GIT_CONFIG_VALUE_1": "false",
	"GIT_CONFIG_KEY_2": "core.hooksPath", "GIT_CONFIG_VALUE_2": "/dev/null",
}

func envMap(env []bwrap.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Key] = e.Value
	}
	return m
}

// TestGitSpecIsValid は、檻の Spec が、bwrap の検証に通り、必要なものだけを見せることを確認する (bwrap は要らない)。
func TestGitSpecIsValid(t *testing.T) {
	for _, tc := range []struct {
		name, state, repo string
		inHome            bool
	}{
		{"状態ディレクトリと元の repo が HOME の下", "/home/tester/.local/state/goro", "/home/tester/work/repo", true},
		{"状態ディレクトリと元の repo が HOME の外", "/data/state/goro", "/data/repos/repo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &Store{root: tc.state + "/sessions", host: bwrap.Host{Home: "/home/tester"}}
			sess := st.layout("20260926-103000-a1b2c3")
			clone := st.cloneBinds(tc.repo, sess)
			exp := st.exportBinds(sess)
			if work := st.workBinds(sess); len(work) != 1 || work[0].Dst != "/work" || !work[0].RW || work[0].Src != sess.Clone {
				t.Errorf("workBinds = %+v, want clone を rw で /work に", work)
			}
			for _, binds := range [][]bwrap.Bind{clone, exp} {
				spec := st.gitSpec(binds, "status")
				argv, err := spec.Argv()
				if err != nil {
					t.Fatalf("Argv: %v", err)
				}
				if spec.Cmd[0] != gitBin || !spec.NewSession || spec.Stdin != nil {
					t.Errorf("Cmd[0] = %q NewSession=%v, want git・NewSession (制御端末を持たせない)", spec.Cmd[0], spec.NewSession)
				}
				if !slices.Contains(argv, "--new-session") {
					t.Error("argv に --new-session が無い")
				}
				if slices.Contains(argv, "--share-net") {
					t.Error("ネットワークを共有している")
				}
				if got := envMap(spec.Env); !reflect.DeepEqual(got, wantGitEnv) {
					t.Errorf("Env = %v, want %v (これだけ。ホストの環境変数は渡らない)", got, wantGitEnv)
				}
				if !reflect.DeepEqual(spec.Tmpfs, []string{"/tmp", "/home/goro"}) || spec.Chdir != "/tmp" {
					t.Errorf("Tmpfs = %v, Chdir = %q, want [/tmp /home/goro]・/tmp (HOME と /tmp は使い捨て)", spec.Tmpfs, spec.Chdir)
				}
				if len(spec.Symlinks) != 4 {
					t.Errorf("Symlinks = %v, want /lib /lib64 /bin /sbin", spec.Symlinks)
				}
			}
			byDst := func(binds []bwrap.Bind) map[string]bwrap.Bind {
				m := map[string]bwrap.Bind{}
				for _, b := range st.gitSpec(binds).Binds {
					m[b.Dst] = b
				}
				return m
			}
			c, e := byDst(clone), byDst(exp)
			if !(!c["/src"].RW && c["/work"].RW && !c["/usr"].RW && c["/out"] == bwrap.Bind{}) {
				t.Errorf("clone の檻: /src は ro・/work は rw・/usr は ro・/out は無い: %+v", c)
			}
			if !(!e["/work"].RW && e["/out"].RW && e["/src"] == bwrap.Bind{}) {
				t.Errorf("export の檻: /work は ro・/out は rw・/src は無い: %+v", e)
			}
			if c["/work"].InHome != tc.inHome || c["/src"].InHome != tc.inHome || e["/out"].InHome != tc.inHome || c["/usr"].InHome {
				t.Errorf("InHome: /work %v /src %v /out %v /usr %v, want %v (/usr は false)", c["/work"].InHome, c["/src"].InHome, e["/out"].InHome, c["/usr"].InHome, tc.inHome)
			}
			if len(c) != 3 || len(e) != 3 {
				t.Errorf("見せるのは /usr と 2 つだけ: %d %d", len(c), len(e))
			}
		})
	}
}

// TestNoExecImport は、session パッケージ (テスト以外) が、os/exec を import しないことを確認する (I1。ホストでは、git を
// 実行しない。プロセスは、sandbox/bwrap の Start だけが起動する)。archtest は cmd/** に os/exec を許すので、ここで別に守る。
func TestNoExecImport(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("ソースを探せない: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" || strings.HasPrefix(p, "os/exec/") || p == "syscall" || p == "unsafe" || p == "plugin" {
				t.Errorf("%s が %s を import している (session は、プロセスを起動しない)", f, p)
			}
		}
	}
}

func TestBoundedBuffer(t *testing.T) {
	var b boundedBuffer
	big := strings.Repeat("x", maxCapture*3)
	if n, err := b.Write([]byte(big)); n != len(big) || err != nil {
		t.Errorf("Write = %d, %v, want 全部 (捨てても、書いたことにする)", n, err)
	}
	if got := len(b.text()); got != maxCapture {
		t.Errorf("持つ長さ = %d, want %d", got, maxCapture)
	}
	var c boundedBuffer
	c.Write([]byte("ok\n\x1b[31mred\x1b[0m\ttab\r\x00\u202e\u2028\u2029\u2066\u2069\u202a\x7f\u0085\u009f\xff end\n"))
	got := c.text()
	for _, bad := range []string{"\x1b", "\r", "\x00", "\u202e", "\u202a", "\u2028", "\u2029", "\u2066", "\u2069", "\x7f", "\u0085", "\u009f", "\xff", "\ufffd"} {
		if strings.Contains(got, bad) {
			t.Errorf("text に制御文字 %q が残った: %q", bad, got)
		}
	}
	if !strings.Contains(got, "ok\n") || !strings.Contains(got, "\ttab") || !strings.Contains(got, "end") {
		t.Errorf("text = %q, want 改行・タブ・通常の文字は残す", got)
	}
}

func TestDefaultStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if got, err := DefaultStateDir(); err != nil || got != "/xdg/state/goro" {
		t.Errorf("XDG_STATE_HOME あり: %q, %v", got, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative/state") // 絶対でなければ、使わない (XDG の仕様)
	t.Setenv("HOME", "/home/u")
	if got, err := DefaultStateDir(); err != nil || got != "/home/u/.local/state/goro" {
		t.Errorf("XDG_STATE_HOME が相対: %q, %v", got, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got, err := DefaultStateDir(); err != nil || got != "/home/u/.local/state/goro" {
		t.Errorf("XDG_STATE_HOME なし: %q, %v", got, err)
	}
}

// TestBindInHome は、InHome が、HOME と等しい path と、その下の path だけに付き、名前が似ているだけの path には付かないことを確認する。
func TestBindInHome(t *testing.T) {
	st := &Store{host: bwrap.Host{Home: "/home/tester"}}
	for path, want := range map[string]bool{
		"/home/tester": true, "/home/tester/x": true, "/home/tester/.local/state/goro/s": true,
		"/home/testerX/y": false, "/home/teste": false, "/home": false, "/home/tester2": false, "/usr": false, "/": false,
	} {
		if got := st.bind(path, "/x", false).InHome; got != want {
			t.Errorf("bind(%q).InHome = %v, want %v", path, got, want)
		}
	}
}

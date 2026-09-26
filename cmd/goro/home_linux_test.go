package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// homeTestProfile は、ensureHome の確認用の profile: 種 2 つ・認証情報の symlink 2 つ。
var homeTestProfile = agentProfile{
	name: "hometest",
	creds: credentials{
		env:     []bwrap.EnvVar{{Key: "HOMETEST_AUTH", Value: jailAuth}},
		linkDir: ".local/share/hometest", files: []string{"auth.json", "mcp-auth.json"},
	},
	seed: []homeFile{{path: ".config/hometest/seed.txt", content: "SEED\n"}, {path: "top.json", content: "{}\n"}},
}

// tree は、dir の下の、ファイルと symlink ("path->先")・ディレクトリ ("path/") を、相対 path で並べる。
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case rel == ".":
		case d.Type()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out = append(out, rel+"->"+target)
		case d.IsDir():
			out = append(out, rel+"/")
		default:
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// ensureHome は、無い HOME を、種と認証情報の symlink (→ 檻の中の /auth/<名前>) つきで作る (0700)。種を置かない (--login) なら、symlink だけ。
// ある HOME は、中を見ない・直さない (檻の状態): 種を書き換えても、消しても、そのまま。
func TestEnsureHome(t *testing.T) {
	dir := shortDir(t)
	homes := filepath.Join(dir, "agents", "hometest", "homes")
	home := filepath.Join(homes, "0123456789abcdef")

	if err := ensureHome(homeTestProfile, home, true); err != nil {
		t.Fatal(err)
	}
	want := []string{".config/", ".config/hometest/", ".config/hometest/seed.txt", ".local/", ".local/share/", ".local/share/hometest/",
		".local/share/hometest/auth.json->/auth/auth.json", ".local/share/hometest/mcp-auth.json->/auth/mcp-auth.json", "top.json"}
	if got := tree(t, home); !slices.Equal(got, want) {
		t.Errorf("HOME の中身:\n got: %q\nwant: %q", got, want)
	}
	for _, d := range []string{filepath.Dir(homes), homes, home} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s の権限 = %v, %v, want 0700", d, fi, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, ".config", "hometest", "seed.txt")); err != nil || string(b) != "SEED\n" {
		t.Errorf("種の中身 = %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(home, "top.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("種の権限 = %v, %v, want 0600", fi, err)
	}
	if left := leftovers(t, homes); len(left) != 0 {
		t.Errorf("一時ディレクトリが残っている: %v", left)
	}

	// ある HOME は、直さない: 種を書き換え、symlink を消して普通のファイルにしても、そのまま (檻の状態を、ホストが上書きしない)。
	if err := os.WriteFile(filepath.Join(home, "top.json"), []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".local", "share", "hometest", "auth.json")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("PER-HOME"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := tree(t, home)
	if err := ensureHome(homeTestProfile, home, true); err != nil {
		t.Fatal(err)
	}
	if after := tree(t, home); !slices.Equal(after, before) {
		t.Errorf("ある HOME が、書き換えられた:\n before: %q\n after: %q", before, after)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "top.json")); string(b) != `{"changed":true}` {
		t.Errorf("ある HOME の種が、上書きされた: %q", b)
	}

	// 種を置かない (--login の HOME): 認証情報の symlink だけ。
	login := filepath.Join(dir, "agents", "hometest", "login-home")
	if err := ensureHome(homeTestProfile, login, false); err != nil {
		t.Fatal(err)
	}
	wantLogin := []string{".local/", ".local/share/", ".local/share/hometest/",
		".local/share/hometest/auth.json->/auth/auth.json", ".local/share/hometest/mcp-auth.json->/auth/mcp-auth.json"}
	if got := tree(t, login); !slices.Equal(got, wantLogin) {
		t.Errorf("ログイン用の HOME の中身:\n got: %q\nwant: %q", got, wantLogin)
	}

	// 認証情報の共有も種も無い profile: 空の HOME。
	bare := filepath.Join(homes, "ffffffffffffffff")
	if err := ensureHome(agentProfile{name: "bare"}, bare, true); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, bare); len(got) != 0 {
		t.Errorf("何も無い profile の HOME = %q", got)
	}
}

// leftovers は、homes の下の、一時ディレクトリ (.new-*) の名前。
func leftovers(t *testing.T, homes string) []string {
	t.Helper()
	ents, err := os.ReadDir(homes)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".new-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// HOME でないものが、HOME の場所にあるとき (ファイル・symlink・ディレクトリへの symlink) は、断る: symlink の先を、HOME として使わない。
func TestEnsureHomeRefusesNonDirectory(t *testing.T) {
	dir := shortDir(t)
	homes := filepath.Join(dir, "homes")
	if err := os.MkdirAll(homes, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(homes, "aaaaaaaaaaaaaaaa")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(homes, "bbbbbbbbbbbbbbbb")
	if err := os.Symlink(elsewhere, link); err != nil { // ディレクトリへの symlink
		t.Fatal(err)
	}
	dangling := filepath.Join(homes, "cccccccccccccccc")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, link, dangling} {
		if err := ensureHome(homeTestProfile, p, true); err == nil {
			t.Errorf("%s を、HOME として受け付けた", p)
		}
	}
	if got := tree(t, elsewhere); len(got) != 0 {
		t.Errorf("symlink の先に、何かを作った: %q", got)
	}
	if left := leftovers(t, homes); len(left) != 0 {
		t.Errorf("一時ディレクトリが残っている: %v", left)
	}
}

// profile の書き間違い (種・symlink の path が HOME の外を指す・名前が 1 つでない) は、作る前に断り、途中のものを残さない。
func TestEnsureHomeRejectsBadProfileWithoutLeftovers(t *testing.T) {
	for name, p := range map[string]agentProfile{
		"種が外を指す":     {name: "x", seed: []homeFile{{path: "../escape", content: "x"}}},
		"種が絶対 path":  {name: "x", seed: []homeFile{{path: "/etc/x", content: "x"}}},
		"linkDir が外": {name: "x", creds: credentials{linkDir: "../out", files: []string{"a"}}},
		"linkDir が空": {name: "x", creds: credentials{files: []string{"a"}}},
		"ファイル名に /":   {name: "x", creds: credentials{linkDir: "d", files: []string{"a/b"}}},
		"ファイル名が ..":  {name: "x", creds: credentials{linkDir: "d", files: []string{".."}}},
		"ファイル名が空":    {name: "x", creds: credentials{linkDir: "d", files: []string{""}}},
		"ファイル名が linkDir の外 (symlink が作れてしまう)": {name: "x", creds: credentials{linkDir: "d", files: []string{"../evil"}}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := shortDir(t)
			homes := filepath.Join(dir, "homes")
			home := filepath.Join(homes, "0123456789abcdef")
			if err := ensureHome(p, home, true); err == nil {
				t.Fatal("受け付けた")
			}
			if _, err := os.Lstat(home); err == nil {
				t.Error("失敗したのに、HOME ができている")
			}
			if left := leftovers(t, homes); len(left) != 0 {
				t.Errorf("一時ディレクトリが残っている: %v", left)
			}
			if _, err := os.Lstat(filepath.Join(dir, "escape")); err == nil {
				t.Error("HOME の外にファイルができた")
			}
		})
	}
}

// 同じ HOME を同時に用意しても、1 つの完全な HOME ができる (途中のものを、別の呼び出しが使わない・一時ディレクトリが残らない)。
func TestEnsureHomeConcurrent(t *testing.T) {
	dir := shortDir(t)
	homes := filepath.Join(dir, "homes")
	home := filepath.Join(homes, "0123456789abcdef")
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = ensureHome(homeTestProfile, home, true)
			if errs[i] == nil { // 戻ったとき、HOME は完全 (種も symlink もある)
				if _, err := os.Lstat(filepath.Join(home, ".local", "share", "hometest", "auth.json")); err != nil {
					errs[i] = err
				}
				if _, err := os.Stat(filepath.Join(home, "top.json")); err != nil {
					errs[i] = err
				}
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("%d 番目: %v", i, err)
		}
	}
	if left := leftovers(t, homes); len(left) != 0 {
		t.Errorf("一時ディレクトリが残っている: %v", left)
	}
	var names []string
	ents, _ := os.ReadDir(homes)
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{".lock", "0123456789abcdef"}) {
		t.Errorf("homes の中 = %q", names)
	}
}

// 本物の profile の、認証情報の共有と種のデータ (実測に基づく。変えるときは、実測し直す)。
// claude: 認証情報の置き場を環境変数で認証用ディレクトリに移す (symlink は使わない: 認証情報は rename で書かれ、symlink を壊す)。
//
//	種は、初回の設定を飛ばす hasCompletedOnboarding だけ (アカウントの情報 oauthAccount は、起動のたびに認証情報から作り直されるので、入れない)。
//
// opencode: auth.json・mcp-auth.json は、その場で書かれる (同じ inode) ので、HOME からの symlink。環境変数は無い。種は無い。
func TestRealProfilesCredentialsAndSeed(t *testing.T) {
	if got := claudeProfile.creds; len(got.env) != 1 || got.env[0] != (bwrap.EnvVar{Key: "CLAUDE_SECURESTORAGE_CONFIG_DIR", Value: jailAuth}) ||
		got.linkDir != "" || len(got.files) != 0 {
		t.Errorf("claude の creds = %+v", got)
	}
	if len(claudeProfile.seed) != 1 || claudeProfile.seed[0].path != ".claude.json" {
		t.Fatalf("claude の seed = %+v", claudeProfile.seed)
	}
	var seed map[string]any
	if err := json.Unmarshal([]byte(claudeProfile.seed[0].content), &seed); err != nil {
		t.Fatalf("claude の種が、JSON でない: %v", err)
	}
	if len(seed) != 1 || seed["hasCompletedOnboarding"] != true {
		t.Errorf("claude の種 = %v, want hasCompletedOnboarding だけ (oauthAccount・userID・projects などを、種にしない)", seed)
	}

	if got := opencodeProfile.creds; len(got.env) != 0 || got.linkDir != ".local/share/opencode" || !slices.Equal(got.files, []string{"auth.json", "mcp-auth.json"}) {
		t.Errorf("opencode の creds = %+v", got)
	}
	if len(opencodeProfile.seed) != 0 {
		t.Errorf("opencode の seed = %+v, want 無し (opencode に onboarding は無い)", opencodeProfile.seed)
	}
}

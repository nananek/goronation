package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/goronation/cmd/internal/session"
)

// 結合テスト (bwrap が要る): エージェントの HOME は repo ごとに分かれ、認証情報だけが、エージェントごとの 1 か所 (auth/) から、全 repo の檻に渡る。
// 偽のエージェント (claude・第 3 の fakeagent) が、HOME (/home/goro) と認証用ディレクトリ (/auth) に、読み書きする。

// newRepo は、f.dir の下に、コミットが 1 つある repo (name) を作る。
func (f *runFixture) newRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(f.dir, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(t, dir, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(t, dir, "add", ".")
	f.git(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// homeOf は、repo (path) の、エージェント name の HOME (ホスト側の path)。キーは、実 path (symlink を解決) から作る。
func (f *runFixture) homeOf(t *testing.T, agent, repo string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return f.agentPath(agent, "homes", session.RepoKey(real))
}

// out は、goro run を args で動かし (成功を要求)、偽のエージェントの "操作 => 結果" の行を返す。
func (f *runFixture) out(t *testing.T, args ...string) map[string]string {
	t.Helper()
	_, res := parseOut(f.goro(t, args...).mustOK(t).stdout)
	return res
}

const seedClaudeJSON = `"{\"hasCompletedOnboarding\": true}\n"` // 種の .claude.json を、%q で引用したもの

// HOME は repo ごと: 別の repo の履歴・メモリ・.claude.json (のプロジェクトの項目) は、見えない。同じ repo のセッションは、HOME を共有する
// (メモリが続く)。--session は、記録した repo のキーの HOME を使う (repo を移動・改名しても、同じセッションは同じ HOME)。
// 同じ repo を、別の名前 (symlink) で指しても、同じ HOME。
func TestRunHomePerRepo(t *testing.T) {
	f := newRunFixture(t)
	repoB := f.newRepo(t, "repoB")
	alias := filepath.Join(f.dir, "alias-of-A")
	if err := os.Symlink(f.repo, alias); err != nil {
		t.Fatal(err)
	}

	// repo A のセッションが、履歴・メモリ・.claude.json のプロジェクトの項目を書く (claude が、cwd = /work をキーに、HOME に残すもの)。
	histA := ".claude/projects/-work/a.jsonl"
	claudeJSONA := `{"projects":{"/work":{"note":"A-PROJECT"}}}`
	first := f.goro(t, "run", "--repo", f.repo, "--", "hwrite", histA, "A-HISTORY", "mem.txt", "A-MEMORY", ".claude.json", claudeJSONA).mustOK(t)
	idA := sessionID(t, first)
	if _, res := parseOut(first.stdout); res["hwrite:mem.txt"] != "ok" || res["hwrite:"+histA] != "ok" {
		t.Fatalf("A の HOME に書けない: %v", res)
	}
	homeA := f.homeOf(t, "claude", f.repo)
	if b, err := os.ReadFile(filepath.Join(homeA, "mem.txt")); err != nil || string(b) != "A-MEMORY" {
		t.Fatalf("A の HOME (%s) に、A のメモリが無い: %q, %v", homeA, b, err)
	}

	// (a) repo B のセッションの檻から、A の履歴・メモリ・.claude.json の A の項目は、見えない。B の .claude.json は、種のまま。
	res := f.out(t, "run", "--repo", repoB, "--", "hread", histA, "mem.txt", ".claude.json")
	for _, name := range []string{histA, "mem.txt"} {
		if !strings.HasPrefix(res["hread:"+name], "err: ") {
			t.Errorf("B の檻から、A の %s が読める: %q", name, res["hread:"+name])
		}
	}
	if res["hread:.claude.json"] != seedClaudeJSON {
		t.Errorf("B の .claude.json = %s, want 種のまま %s (A の項目 A-PROJECT が見えてはいけない)", res["hread:.claude.json"], seedClaudeJSON)
	}
	if got := f.out(t, "run", "--repo", repoB, "--", "hls")["hls"]; got != ".claude.json" {
		t.Errorf("B の HOME の中身 = %q, want .claude.json だけ (種)", got)
	}
	res = f.out(t, "run", "--repo", repoB, "--", "probe", "stat:"+homeA, "stat:"+filepath.Join(homeA, "mem.txt"), "stat:"+f.agentPath("claude", "homes"),
		"stat:"+f.stateDir(), "mnt:/home/goro", "mnt:/auth")
	for _, op := range []string{"stat:" + homeA, "stat:" + filepath.Join(homeA, "mem.txt"), "stat:" + f.agentPath("claude", "homes"), "stat:" + f.stateDir()} {
		if !strings.HasPrefix(res[op], "err") {
			t.Errorf("B の檻から、ホストの %s が見える: %q", op, res[op])
		}
	}
	if res["mnt:/home/goro"] != "rw" || res["mnt:/auth"] != "rw" {
		t.Errorf("HOME・認証用ディレクトリの mount = %q・%q, want rw・rw", res["mnt:/home/goro"], res["mnt:/auth"])
	}
	homeB := f.homeOf(t, "claude", repoB)
	if homeA == homeB {
		t.Fatalf("別の repo の HOME が同じ: %s", homeA)
	}

	// (b) 同じ repo の別のセッション (新しい --repo A) は、HOME を共有する: 履歴・メモリ・.claude.json が続く。symlink 越しの repo も、同じ HOME。
	for name, repo := range map[string]string{"同じ path": f.repo, "symlink 越し": alias} {
		res = f.out(t, "run", "--repo", repo, "--", "hread", histA, "mem.txt", ".claude.json")
		if res["hread:"+histA] != `"A-HISTORY"` || res["hread:mem.txt"] != `"A-MEMORY"` || !strings.Contains(res["hread:.claude.json"], "A-PROJECT") {
			t.Errorf("%s: 同じ repo の別のセッションに、A の HOME が見えない: %v", name, res)
		}
	}
	if keys := f.homeKeys(t, "claude"); len(keys) != 2 {
		t.Errorf("HOME の数 = %v, want 2 (repo A と B。symlink 越しの A は、A と同じ)", keys)
	}

	// (e) --session は、記録のキーの HOME を使う (--repo が無くても)。repo を移動・改名しても、同じセッションは、同じ HOME。
	if got := f.out(t, "run", "--session", idA, "--", "hread", "mem.txt")["hread:mem.txt"]; got != `"A-MEMORY"` {
		t.Errorf("--session で、A の HOME が見えない: %q", got)
	}
	moved := f.repo + "-moved"
	if err := os.Rename(f.repo, moved); err != nil {
		t.Fatal(err)
	}
	if got := f.out(t, "run", "--session", idA, "--", "hread", "mem.txt")["hread:mem.txt"]; got != `"A-MEMORY"` {
		t.Errorf("repo を移動した後の --session で、A の HOME が見えない: %q", got)
	}
	// 移動した後の path で --repo を指すと、別の repo (別の実 path) として、別の HOME になる (記録のキーは、変わらない)。
	if got := f.out(t, "run", "--repo", moved, "--", "hread", "mem.txt")["hread:mem.txt"]; !strings.HasPrefix(got, "err: ") {
		t.Errorf("別の実 path の repo に、A の HOME が見える: %q", got)
	}
	keys := f.homeKeys(t, "claude")
	if len(keys) != 3 {
		t.Errorf("HOME の数 = %v, want 3", keys)
	}
	for _, k := range keys {
		if fi, err := os.Stat(f.agentPath("claude", "homes", k)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("HOME %s の権限 = %v, %v, want 0700", k, fi, err)
		}
	}
	// 旧配置 (エージェントで 1 つの HOME: agents/<name>/home) は、作らない。
	if _, err := os.Lstat(f.agentPath("claude", "home")); err == nil {
		t.Error("廃止した共有の HOME (agents/claude/home) が作られた")
	}
}

// 認証情報は、エージェントごとの 1 か所 (auth/) から、全 repo の檻に渡る。ログイン 1 回で、どの repo でも使える。
// (1) 再ログイン (--login) の後は、別の repo の HOME も、新しい認証情報を使う (次のセッションから。実行中のセッションにも、次の読み込みから届く)。
// (2) セッション中の更新 (refresh) は、実体 (auth/) に戻り、別の repo の (実行中の) 檻からも使える。rename で置き換える書き方 (claude) でも、壊れない。
// (d) 認証情報以外 (HOME の中身) は、共有されない。
func TestRunCredentialsSharedAcrossRepos(t *testing.T) {
	f := newRunFixture(t)
	repoB := f.newRepo(t, "repoB")
	hostCred := f.agentPath("claude", "auth", "cred.json")
	readHost := func() string {
		b, _ := os.ReadFile(hostCred)
		return string(b)
	}

	// ログインは 1 回: 認証情報が、認証用ディレクトリに残る。ログイン用の HOME (login-home) には、残らない。
	f.goro(t, "run", "--login", "--", "awrite", "cred.json", "V1").mustOK(t)
	if got := readHost(); got != "V1" {
		t.Fatalf("認証情報が、認証用ディレクトリ (%s) に無い: %q", hostCred, got)
	}
	// どの repo の HOME からも使える (repo の HOME は、ログインした HOME とは別)。認証用ディレクトリを指す環境変数も、渡る。
	for name, repo := range map[string]string{"repo A": f.repo, "repo B": repoB} {
		if got := f.out(t, "run", "--repo", repo, "--", "aread", "cred.json")["aread:cred.json"]; got != `"V1"` {
			t.Errorf("%s の檻から、認証情報が使えない: %q", name, got)
		}
		if got := f.out(t, "run", "--repo", repo, "--", "env", "CLAUDE_SECURESTORAGE_CONFIG_DIR")["env:CLAUDE_SECURESTORAGE_CONFIG_DIR"]; got != jailAuth {
			t.Errorf("%s の檻の CLAUDE_SECURESTORAGE_CONFIG_DIR = %q, want %s", name, got, jailAuth)
		}
	}

	// (d) 認証情報以外は、共有されない: A の HOME のファイルは、B の HOME からも、認証用ディレクトリからも見えない。
	f.out(t, "run", "--repo", f.repo, "--", "hwrite", "other.txt", "A-ONLY")
	if got := f.out(t, "run", "--repo", repoB, "--", "hread", "other.txt")["hread:other.txt"]; !strings.HasPrefix(got, "err: ") {
		t.Errorf("A の HOME のファイルが、B の HOME に見える: %q", got)
	}
	if got := f.out(t, "run", "--repo", f.repo, "--", "als")["als"]; strings.Contains(got, "other.txt") || !strings.Contains(got, "cred.json") {
		t.Errorf("認証用ディレクトリの中身 = %q (認証情報だけ。HOME のファイルは入らない)", got)
	}
	if _, err := os.Stat(filepath.Join(f.agentPath("claude", "auth"), "other.txt")); err == nil {
		t.Error("HOME のファイルが、認証用ディレクトリに現れた")
	}

	// (1) 再ログイン: 次に起動する、別の repo の (別の HOME の) セッションから、新しい認証情報が使われる。
	f.goro(t, "run", "--login", "--", "awrite", "cred.json", "V2").mustOK(t)
	if got := f.out(t, "run", "--repo", repoB, "--", "aread", "cred.json")["aread:cred.json"]; got != `"V2"` {
		t.Errorf("再ログインの後、別の repo の HOME が、古い認証情報 (%s) を使う", got)
	}
	// 実行中のセッションにも、再ログインが伝わる (同じディレクトリを見ている): 実行中の B の檻が、V2 → V3 の変化を、再起動なしで見る。
	running := f.start(t, "run", "--repo", repoB, "--", "await", "cred.json", "V2")
	running.waitStdout("ready")
	f.goro(t, "run", "--login", "--", "awrite", "cred.json", "V3").mustOK(t)
	running.waitStdout(`changed="V3"`)
	if code := running.wait(); code != 0 {
		t.Errorf("実行中の檻の終了コード = %d", code)
	}

	// (2) セッション中の更新: A の檻が、認証情報を (rename で置き換える書き方で) 更新する → 実体 (auth/) に戻り、実行中の B の檻にも、次の B の起動にも届く。
	running = f.start(t, "run", "--repo", repoB, "--", "await", "cred.json", "V3")
	running.waitStdout("ready")
	if got := f.out(t, "run", "--repo", f.repo, "--", "arename", "cred.json", "REFRESHED")["arename:cred.json"]; got != "ok" {
		t.Fatalf("認証用ディレクトリで、rename で置き換えられない (ディレクトリの bind のはず): %q", got)
	}
	if got := readHost(); got != "REFRESHED" {
		t.Errorf("A の更新が、実体 (%s) に戻っていない: %q", hostCred, got)
	}
	running.waitStdout(`changed="REFRESHED"`)
	if code := running.wait(); code != 0 {
		t.Errorf("実行中の檻の終了コード = %d", code)
	}
	if got := f.out(t, "run", "--repo", repoB, "--", "aread", "cred.json")["aread:cred.json"]; got != `"REFRESHED"` {
		t.Errorf("A の更新が、次の B の檻に届いていない: %q", got)
	}
	// 同時に動く 2 つの (別の repo の) セッションが、どちらも、同じ認証情報を見る (片方が更新すると、もう片方も新しいものを見る)。
	both := f.start(t, "run", "--repo", f.repo, "--", "await", "cred.json", "REFRESHED")
	both.waitStdout("ready")
	other := f.start(t, "run", "--repo", repoB, "--", "await", "cred.json", "REFRESHED")
	other.waitStdout("ready")
	f.out(t, "run", "--login", "--", "arename", "cred.json", "NEXT")
	both.waitStdout(`changed="NEXT"`)
	other.waitStdout(`changed="NEXT"`)
	both.wait()
	other.wait()

	// login 用の HOME と repo の HOME は、別。認証情報は、どちらの HOME にも残らない (認証用ディレクトリだけ)。
	for _, home := range []string{f.agentPath("claude", "login-home"), f.homeOf(t, "claude", f.repo), f.homeOf(t, "claude", repoB)} {
		for _, name := range []string{"cred.json", "login-marker"} {
			if _, err := os.Lstat(filepath.Join(home, name)); err == nil {
				t.Errorf("認証情報 %s が、HOME %s に残っている", name, home)
			}
		}
	}
}

// ログイン用の HOME は、repo の HOME とは別で、種を置かない (初回の設定を、最後まで通す)。ログインの間の HOME の状態は、次のログインに残る。
func TestRunLoginHomeIsSeparateAndUnseeded(t *testing.T) {
	f := newRunFixture(t)
	if got := f.out(t, "run", "--login", "--", "hls")["hls"]; got != "" {
		t.Errorf("ログイン用の HOME の中身 = %q, want 空 (種を置かない)", got)
	}
	f.out(t, "run", "--login", "--", "hwrite", ".claude.json", "LOGIN-STATE")
	if got := f.out(t, "run", "--login", "--", "hread", ".claude.json")["hread:.claude.json"]; got != `"LOGIN-STATE"` {
		t.Errorf("次のログインに、ログイン用の HOME の状態が残っていない: %q", got)
	}
	// repo の HOME は、ログイン用の HOME を見ない (種のまま)。
	if got := f.out(t, "run", "--repo", f.repo, "--", "hread", ".claude.json")["hread:.claude.json"]; got != seedClaudeJSON {
		t.Errorf("repo の HOME の .claude.json = %s, want 種 %s", got, seedClaudeJSON)
	}
	// ログインの檻からも、repo の HOME は見えない。
	homeA := f.homeOf(t, "claude", f.repo)
	if got := f.out(t, "run", "--login", "--", "probe", "stat:"+homeA)["stat:"+homeA]; !strings.HasPrefix(got, "err") {
		t.Errorf("ログインの檻から、repo の HOME が見える: %q", got)
	}
}

// 認証情報の置き場を変えられないエージェント (opencode 型): HOME に、認証情報の symlink (→ /auth/<名前>) を置く。エージェントが、その場で書くと、
// symlink を辿って認証用ディレクトリに届き、別の repo の HOME からも読める。symlink の外 (データベースなど) は、repo ごと。
// 種は、HOME を作るときだけ置く (ある HOME は、ホストが書き換えない)。--login の HOME には、種が無い。
// (f) profile に足すだけで動く: 第 3 の profile (fakeagent) の creds・seed のデータだけで、この仕組みが働く。
func TestRunCredentialsViaSymlink(t *testing.T) {
	f := newRunFixture(t)
	f.env = append(f.env, "GORO_FAKEAGENT="+f.exe)
	repoB := f.newRepo(t, "repoB")
	link := ".local/share/fakeagent/auth.json"
	fa := func(args ...string) map[string]string {
		return f.out(t, append([]string{"run", "--agent", "fakeagent"}, args...)...)
	}

	// repo A の HOME: 種と、認証情報の symlink (mcp-auth.json も)。
	got := fa("--repo", f.repo, "--", "hls")["hls"]
	want := ".config/fakeagent/seed.txt,.local/share/fakeagent/auth.json->/auth/auth.json,.local/share/fakeagent/mcp-auth.json->/auth/mcp-auth.json"
	if got != want {
		t.Errorf("A の HOME の中身:\n got: %s\nwant: %s", got, want)
	}
	if res := fa("--repo", f.repo, "--", "hread", ".config/fakeagent/seed.txt"); res["hread:.config/fakeagent/seed.txt"] != `"SEED-OF-FAKEAGENT\n"` {
		t.Errorf("種の中身 = %v", res)
	}
	if got := fa("--repo", f.repo, "--", "env", "FAKEAGENT_AUTH_DIR", "FAKEAGENT_MODE"); got["env:FAKEAGENT_AUTH_DIR"] != jailAuth || got["env:FAKEAGENT_MODE"] != "test" {
		t.Errorf("環境変数 = %v", got)
	}

	// A の檻が、symlink 越しに認証情報を書く (その場で書く) → 認証用ディレクトリに届く。symlink は、そのまま。
	if got := fa("--repo", f.repo, "--", "hwrite", link, "KEY-A", ".local/share/fakeagent/opencode.db", "A-DB")[("hwrite:" + link)]; got != "ok" {
		t.Fatalf("symlink 越しに書けない: %q", got)
	}
	homeA := f.homeOf(t, "fakeagent", f.repo)
	if b, err := os.ReadFile(f.agentPath("fakeagent", "auth", "auth.json")); err != nil || string(b) != "KEY-A" {
		t.Errorf("認証情報が、認証用ディレクトリ (agents/fakeagent/auth/auth.json) に届いていない: %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(homeA, link)); err != nil || target != jailAuth+"/auth.json" {
		t.Errorf("書いた後の symlink = %q, %v (symlink のままのはず)", target, err)
	}

	// 別の repo の HOME から、認証情報が読める (symlink 越し)。symlink の外 (データベース) は、A のものが見えない。
	res := fa("--repo", repoB, "--", "hread", link, ".local/share/fakeagent/opencode.db")
	if res["hread:"+link] != `"KEY-A"` || !strings.HasPrefix(res["hread:.local/share/fakeagent/opencode.db"], "err: ") {
		t.Errorf("B の HOME: 認証情報は読め、データベースは見えないはず: %v", res)
	}
	if got := fa("--repo", f.repo, "--", "als")["als"]; got != "auth.json" {
		t.Errorf("認証用ディレクトリの中身 = %q, want auth.json だけ (データベースは、入らない)", got)
	}

	// 再ログイン (--login): login の HOME にも symlink があり (書いた先は共有)、種は無い。次の別の repo の起動に、新しい認証情報が届く。
	if got := fa("--login", "--", "hls")["hls"]; got != ".local/share/fakeagent/auth.json->/auth/auth.json,.local/share/fakeagent/mcp-auth.json->/auth/mcp-auth.json" {
		t.Errorf("ログイン用の HOME の中身 = %q (symlink だけ。種は無い)", got)
	}
	fa("--login", "--", "hwrite", link, "KEY-B") // symlink 越しに書く (fakeagent の loginArgs は login。fake は、続きの場面も動かす)
	if got := fa("--repo", repoB, "--", "hread", link)["hread:"+link]; got != `"KEY-B"` {
		t.Errorf("再ログインの後、別の repo の HOME が、古い認証情報 (%s) を使う", got)
	}

	// 種は、HOME を作るときだけ: 書き換えた種を、次の起動で戻さない。
	fa("--repo", f.repo, "--", "hwrite", ".config/fakeagent/seed.txt", "CHANGED")
	if got := fa("--repo", f.repo, "--", "hread", ".config/fakeagent/seed.txt")["hread:.config/fakeagent/seed.txt"]; got != `"CHANGED"` {
		t.Errorf("ある HOME の種が、戻された: %q", got)
	}
}

// 記録のキーが無い (HOME を repo ごとに分ける前に作った) セッション、記録が壊れたセッションは、--session で使わない (共有の HOME に戻さない・
// 別の repo の HOME を選ばない)。export は、使える (clone は、そのまま)。
func TestRunSessionWithoutOrWithBadHomeKey(t *testing.T) {
	f := newRunFixture(t)
	r := f.goro(t, "run", "--repo", f.repo, "--", "commit", "x.txt", "X", "msg").mustOK(t)
	id := sessionID(t, r)
	keyFile := filepath.Join(f.stateDir(), "sessions", id, "homekey")
	if b, err := os.ReadFile(keyFile); err != nil || len(strings.TrimSpace(string(b))) != 16 {
		t.Fatalf("セッションに、repo のキーが記録されていない: %q, %v", b, err)
	}

	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	resumed := f.goro(t, "run", "--session", id, "--", "info")
	if resumed.code != 1 || !strings.Contains(resumed.stderr, "HOME を repo ごとに分ける前に作った") || !strings.Contains(resumed.stderr, "goro run --repo PATH") {
		t.Errorf("記録の無いセッションの再開:\n%s", resumed)
	}
	for _, bad := range []string{"../../etc/xxxx\n", "0123456789ABCDEF\n", "0123456789abcdef\nextra\n", "\n"} {
		if err := os.WriteFile(keyFile, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := f.goro(t, "run", "--session", id, "--", "info"); r.code != 1 || !strings.Contains(r.stderr, "セッションを使えない") {
			t.Errorf("壊れた記録 %q の再開:\n%s", bad, r)
		}
	}
	if keys := f.homeKeys(t, "claude"); len(keys) != 1 { // 断った再開が、HOME を増やさない
		t.Errorf("HOME の数 = %v, want 1", keys)
	}
	// 記録が無くなっても、成果は取り出せる (clone は、そのまま)。
	f.goro(t, "export", id).mustOK(t)
}

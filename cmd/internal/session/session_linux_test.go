package session

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// このファイルの、檻を起動するテスト (needBwrap を呼ぶもの) は、bwrap の無い環境では skip し、
// GORO_REQUIRE_BWRAP=1 の CI leg では、skip でなく fail する。元のリポジトリは、ホストの git で作る (準備だけ)。

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// copyTree は、src を dst へ、モード・symlink を保って写す (罠の対照実験用。cp -a)。
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if out, err := exec.Command("/bin/cp", "-a", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("cp -a: %v\n%s", err, out)
	}
}

// TestCreateClonesCommittedContentOnly は、Create が、コミット済みの内容だけを持つ、独立した clone を作ることと、
// 元のリポジトリを変えず、ホストで罠を実行しないことを確認する。
func TestCreateClonesCommittedContentOnly(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	st, _, _ := testStore(t)
	before := treeHash(t, src.Dir)
	srcBig, _ := os.ReadFile(filepath.Join(src.Dir, "big.bin"))

	sess, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir, Name: "Alice B", Email: "alice@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}

	// コミット済みの内容だけ。
	if got := readFile(t, filepath.Join(sess.Clone, "README.md")); got != "# src\n" {
		t.Errorf("README.md = %q, want コミット済みの内容 (作業ツリーの編集を持ち込まない)", got)
	}
	if got := readFile(t, filepath.Join(sess.Clone, "notes.txt")); got != "notes\nsecond\n" {
		t.Errorf("notes.txt = %q", got)
	}
	for _, name := range []string{"secret.env", "staged.txt"} {
		if exists(filepath.Join(sess.Clone, name)) {
			t.Errorf("%s が clone にある (未追跡・ステージだけのファイルを持ち込んだ)", name)
		}
	}
	for _, name := range []string{".gitmodules", ".gitattributes", "subdir/file.txt"} {
		if !exists(filepath.Join(sess.Clone, name)) {
			t.Errorf("%s が clone に無い", name)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(sess.Clone, "big.bin")); !bytes.Equal(got, srcBig) {
		t.Errorf("big.bin (8 MiB) が一致しない (%d バイト)", len(got))
	}
	for name, want := range map[string]string{"link": "README.md", "abs-link": "/etc/hostname"} {
		if got, err := os.Readlink(filepath.Join(sess.Clone, name)); err != nil || got != want {
			t.Errorf("symlink %s = %q, %v, want %q", name, got, err, want)
		}
	}
	if got := readFile(t, filepath.Join(sess.Clone, ".git", "HEAD")); got != "ref: refs/heads/main\n" {
		t.Errorf(".git/HEAD = %q", got)
	}

	// 罠 (元の .git/config・hooks) は、clone に無い。origin は無く、名義が設定されている。
	cfg := readFile(t, filepath.Join(sess.Clone, ".git", "config"))
	for _, bad := range []string{"[remote", "fsmonitor", "textconv", "packObjectsHook", "alias", src.Markers, src.Dir} {
		if strings.Contains(cfg, bad) {
			t.Errorf("clone の .git/config に %q がある:\n%s", bad, cfg)
		}
	}
	for _, want := range []string{"name = Alice B", "email = alice@example.invalid"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("clone の .git/config に %q が無い:\n%s", want, cfg)
		}
	}
	hooks, _ := os.ReadDir(filepath.Join(sess.Clone, ".git", "hooks"))
	for _, h := range hooks {
		if !strings.HasSuffix(h.Name(), ".sample") {
			t.Errorf("clone の hooks に、元の hook %q がある", h.Name())
		}
	}

	// 独立: alternates も、ハードリンクも無い。
	if exists(filepath.Join(sess.Clone, ".git", "objects", "info", "alternates")) {
		t.Error("clone に alternates がある (元のリポジトリと object を共有している)")
	}
	for _, dir := range []string{filepath.Join(sess.Clone, ".git", "objects"), filepath.Join(src.Dir, ".git", "objects")} {
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if n := fi.Sys().(*syscall.Stat_t).Nlink; n != 1 {
				t.Errorf("%s のリンク数 = %d, want 1 (ハードリンクで共有している)", p, n)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// セッションのディレクトリは 0700。Get で取れる。
	for _, d := range []string{sess.Dir, sess.Clone, sess.Run, sess.Export} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s = %v, %v, want 0700", d, fi, err)
		}
	}
	if got, err := st.Get(sess.ID); err != nil || got.Clone != sess.Clone {
		t.Errorf("Get = %+v, %v", got, err)
	}

	// 元のリポジトリは、変わらない。ホストでは、罠が実行されていない。
	if after := treeHash(t, src.Dir); after != before {
		t.Error("元のリポジトリが変わった")
	}
	if hit := markersHit(t, src.Markers); len(hit) != 0 {
		t.Errorf("ホストで罠が実行された: %v", hit)
	}
}

// TestCreateSourceWithAlternates は、元のリポジトリが alternates (別の場所の object を借りる) を持つとき、clone が、その借用を
// 引き継がず、object を自分で持つ (元と共有しない) ことを確認する。--no-local でなければ、git clone は、object のディレクトリと
// alternates を、そのまま写してしまう。借りる先は、元のリポジトリの .git の中 (檻から見える場所) にする。
func TestCreateSourceWithAlternates(t *testing.T) {
	needBwrap(t)
	needGit(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Join(repo, "repo")
	os.Mkdir(repo, 0o755)
	hostGit(t, repo, "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("from lender\n"), 0o644)
	hostGit(t, repo, "add", "a.txt")
	hostGit(t, repo, "commit", "-q", "-m", "one")
	// object を .git/extra へ移し、.git/objects は、相対の alternates で、それを借りるだけにする。
	if err := os.Rename(filepath.Join(repo, ".git", "objects"), filepath.Join(repo, ".git", "extra")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(repo, ".git", "objects", "info"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "objects", "info", "alternates"), []byte("../extra\n"), 0o644)
	if got := hostGit(t, repo, "log", "--format=%s"); strings.TrimSpace(got) != "one" {
		t.Fatalf("準備: alternates で借りた repo が読めない: %q", got)
	}

	st, _, _ := testStore(t)
	sess, err := st.Create(ctxT(t), CreateOptions{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(sess.Clone, ".git", "objects", "info", "alternates")) {
		t.Error("clone が、元のリポジトリの alternates を引き継いだ")
	}
	if got := readFile(t, filepath.Join(sess.Clone, "a.txt")); got != "from lender\n" {
		t.Errorf("a.txt = %q", got)
	}
	// clone は、自分で object を持つ (借りていない)。
	n := 0
	filepath.WalkDir(filepath.Join(sess.Clone, ".git", "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && !strings.HasSuffix(p, "alternates") {
			n++
		}
		return nil
	})
	if n == 0 {
		t.Error("clone の .git/objects に、object が無い (借りている)")
	}
}

// TestCreateDefaultIdentity は、名義を指定しないとき、既定の名義になることを確認する。
func TestCreateDefaultIdentity(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	st, _, _ := testStore(t)
	sess, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir})
	if err != nil {
		t.Fatal(err)
	}
	cfg := readFile(t, filepath.Join(sess.Clone, ".git", "config"))
	for _, want := range []string{"name = " + DefaultName, "email = " + DefaultEmail} {
		if !strings.Contains(cfg, want) {
			t.Errorf(".git/config に %q が無い:\n%s", want, cfg)
		}
	}
}

// TestCreateFromRepoUnderHome は、元の repo が HOME の下にあっても (InHome を明示する)、動くことを確認する。
func TestCreateFromRepoUnderHome(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	st, home, _ := testStore(t)
	repo := filepath.Join(home, "work", "repo")
	if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	copyTree(t, src.Dir, repo)
	sess, err := st.Create(ctxT(t), CreateOptions{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(sess.Clone, "README.md")); got != "# src\n" {
		t.Errorf("README.md = %q", got)
	}
}

// TestCreateFailureLeavesNothing は、Create が失敗したとき、セッションのディレクトリを残さないことを確認する。
func TestCreateFailureLeavesNothing(t *testing.T) {
	needBwrap(t)
	st, home, _ := testStore(t)
	notRepo := t.TempDir()
	secret := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, repo := range map[string]string{"git のリポジトリではない": notRepo, "機密の path (~/.ssh)": secret, "存在しない": filepath.Join(notRepo, "none")} {
		if s, err := st.Create(ctxT(t), CreateOptions{Repo: repo}); err == nil {
			t.Errorf("%s: Create が error にならない (%+v)", name, s)
		}
		if ents, _ := os.ReadDir(st.root); len(ents) != 0 {
			t.Errorf("%s: 失敗したのに、セッションが残った: %v", name, ents)
		}
	}
	// 名義・URL などの入力の誤りは、檻を起動する前に断る。
	for _, o := range []CreateOptions{{Repo: "https://example.com/r.git"}, {Repo: notRepo, Name: "a\nb"}, {Repo: notRepo, Email: "no-at"}, {}} {
		if _, err := st.Create(ctxT(t), o); err == nil {
			t.Errorf("Create(%+v) が error にならない", o)
		}
		if ents, _ := os.ReadDir(st.root); len(ents) != 0 {
			t.Errorf("入力の誤りで、セッションが作られた: %v", ents)
		}
	}
}

// TestTrapsWouldRunOnHost は、対照実験: 罠は、ホストで git を実行すれば、実際に発火する (fsmonitor・alias・textconv・hooks・
// clean フィルタ)。これが起きるなら、他のテストの「ホストに目印が無い」は、意味を持つ。
func TestTrapsWouldRunOnHost(t *testing.T) {
	src := newSrcRepo(t)
	dst := filepath.Join(t.TempDir(), "copy")
	copyTree(t, src.Dir, dst)
	hostGitErr(t, dst, "status")
	hostGitErr(t, dst, "evil")
	hostGitErr(t, dst, "show", "HEAD")
	hostGitErr(t, dst, "commit", "--allow-empty", "-m", "x")
	hostGitErr(t, dst, "add", "-A")
	hit := markersHit(t, src.Markers)
	for _, want := range []string{"fsmonitor", "alias", "textconv", "hooksdir", "clean"} {
		if !slices.Contains(hit, want) {
			t.Errorf("罠 %q が、ホストの git で発火しない (対照実験が成り立たない)。発火したもの: %v", want, hit)
		}
	}
}

// exportFixture は、罠を仕込んだ元の repo から Create し、檻の中で、コミットとブランチと罠を clone に足す (檻の中の AI が、
// clone の .git/config・hooks を書き換えるのを模す)。
func exportFixture(t *testing.T) (*Store, srcRepo, *Session) {
	t.Helper()
	needBwrap(t)
	src := newSrcRepo(t)
	st, _, _ := testStore(t)
	sess, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir})
	if err != nil {
		t.Fatal(err)
	}
	var hooks strings.Builder
	for _, h := range hookNames {
		hooks.WriteString("printf '#!/bin/sh\\n: > " + src.Markers + "/ran-hooksdir\\n' > .git/hooks/" + h + "; chmod +x .git/hooks/" + h + "\n")
	}
	cageSh(t, st, sess, `set -e
echo 'hello from cage' > cage.txt
git add cage.txt
git commit -q -m 'commit from cage'
git branch feature/x
git branch 'evil;$(touch$IFS/tmp/pwn)'
mkdir -p .git/hooks
`+hooks.String()+`cat >> .git/config <<'TRAPS'
`+trapConfig(src.Markers)+`TRAPS
`)
	return st, src, sess
}

// TestExportBundle は、Export が、檻の中で bundle を作り、ホストでは git を実行せずに、その path と取り込みのコマンドを返すことと、
// 利用者の git fetch で取り込めることを確認する。
func TestExportBundle(t *testing.T) {
	st, src, sess := exportFixture(t)
	if hit := markersHit(t, src.Markers); len(hit) != 0 {
		t.Fatalf("準備の段階で罠が発火した: %v", hit)
	}

	b, err := st.Export(ctxT(t), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(sess.Export, "goro.bundle"); b.Path != want {
		t.Errorf("Path = %q, want %q", b.Path, want)
	}
	if fi, err := os.Stat(b.Path); err != nil || fi.Size() != b.Size || b.Size == 0 {
		t.Errorf("Size = %d, ファイル = %v, %v", b.Size, fi, err)
	}
	if want := []string{"refs/heads/feature/x", "refs/heads/main"}; !slices.Equal(sorted(b.Heads), want) {
		t.Errorf("Heads = %q, want %q", b.Heads, want)
	}
	if b.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (名前が安全でないブランチ)", b.Skipped)
	}
	if !strings.Contains(b.Fetch, "-c transfer.fsckObjects=true fetch ") {
		t.Errorf("Fetch に、fsck の指定が無い: %s", b.Fetch)
	}
	for _, bad := range []string{"evil", "$(", ";", "pwn"} {
		if strings.Contains(b.Fetch, bad) {
			t.Errorf("Fetch に、檻が決めた名前 %q が入っている: %s", bad, b.Fetch)
		}
	}

	// ホストでは、罠が実行されていない。対照: 同じ clone を、ホストの git で動かせば、発火する。
	if hit := markersHit(t, src.Markers); len(hit) != 0 {
		t.Fatalf("Export で、ホストの罠が実行された: %v", hit)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	copyTree(t, sess.Clone, dst)
	hostGitErr(t, dst, "status")
	hostGitErr(t, dst, "evil")
	hostGitErr(t, dst, "commit", "--allow-empty", "-m", "x")
	if hit := markersHit(t, src.Markers); !slices.Contains(hit, "fsmonitor") || !slices.Contains(hit, "alias") || !slices.Contains(hit, "hooksdir") {
		t.Errorf("clone の罠が、ホストの git で発火しない (対照実験が成り立たない): %v", hit)
	}
	for _, n := range trapNames {
		os.Remove(filepath.Join(src.Markers, "ran-"+n))
	}

	// 利用者の取り込み: 別の repo で、返されたコマンドをそのまま実行する (シェル。設定を固定する)。
	user := filepath.Join(t.TempDir(), "user")
	os.Mkdir(user, 0o755)
	hostGit(t, user, "init", "-q", "-b", "main", ".")
	cmd := exec.Command("/bin/sh", "-c", b.Fetch)
	cmd.Dir = user
	cmd.Env = hostEnv(t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("取り込みのコマンドが失敗した: %v\n%s\n%s", err, b.Fetch, out)
	}
	refs := strings.Fields(hostGit(t, user, "for-each-ref", "--format=%(refname)"))
	prefix := "refs/heads/goro/" + sess.ID + "/"
	if want := []string{prefix + "feature/x", prefix + "main"}; !slices.Equal(sorted(refs), want) {
		t.Errorf("取り込んだ ref = %q, want %q", refs, want)
	}
	if got := hostGit(t, user, "log", "--format=%s", prefix+"main"); !strings.Contains(got, "commit from cage") || !strings.Contains(got, "init") {
		t.Errorf("取り込んだ履歴 = %q", got)
	}
	if got := hostGit(t, user, "show", prefix+"main:cage.txt"); got != "hello from cage\n" {
		t.Errorf("cage.txt = %q", got)
	}
	hostGit(t, user, "fsck", "--strict")

	// 2 回目の Export も動く (前の bundle を置き換える)。取り込みも、そのまま通る。
	b2, err := st.Export(ctxT(t), sess.ID)
	if err != nil || b2.Path != b.Path {
		t.Fatalf("2 回目の Export: %+v, %v", b2, err)
	}
	cmd = exec.Command("/bin/sh", "-c", b2.Fetch)
	cmd.Dir = user
	cmd.Env = hostEnv(t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("2 回目の取り込みが失敗した: %v\n%s", err, out)
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// hugeHeader は、空行で終わる、64 KiB を超えるヘッダ (正しい形の ref の行が、大量に続く)。上限で読みきれないので、拒否する。
func hugeHeader() string {
	var b strings.Builder
	b.WriteString("# v2 git bundle\n")
	for i := 0; b.Len() < maxHeader+1000; i++ {
		b.WriteString(oid + " refs/heads/branch-" + strings.Repeat("x", 40) + "-" + strconv.Itoa(i) + "\n")
	}
	b.WriteString("\nPACK")
	return b.String()
}

// TestExportRejectsBadBundle は、bundle が、通常のファイルでない・大きすぎる・ヘッダが正しくない・ブランチが無いなら、
// Export が拒否することを確認する。檻が、bundle の名前を差し替える場面を、afterBundle で作る。
func TestExportRejectsBadBundle(t *testing.T) {
	st, _, sess := exportFixture(t)
	valid := "# v2 git bundle\n" + oid + " refs/heads/main\n\nPACK"
	other := filepath.Join(t.TempDir(), "other")
	os.WriteFile(other, []byte(valid), 0o644)
	replace := func(mk func(p string) error) func(dir string) {
		return func(dir string) {
			p := filepath.Join(dir, bundleName)
			if err := os.Remove(p); err != nil {
				t.Error(err)
			}
			if err := mk(p); err != nil {
				t.Error(err)
			}
		}
	}
	write := func(content string) func(dir string) {
		return replace(func(p string) error { return os.WriteFile(p, []byte(content), 0o644) })
	}
	for _, tc := range []struct {
		name string
		hook func(dir string)
		want string
	}{
		{"symlink (外のファイルへ)", replace(func(p string) error { return os.Symlink(other, p) }), "通常のファイルではない"},
		{"symlink (/etc/passwd へ)", replace(func(p string) error { return os.Symlink("/etc/passwd", p) }), "通常のファイルではない"},
		{"symlink (export の中の別ファイルへ)", replace(func(p string) error { return os.Symlink("other.bundle", p) }), "通常のファイルではない"},
		{"FIFO", replace(func(p string) error { return syscall.Mkfifo(p, 0o644) }), "通常のファイルではない"},
		{"ディレクトリ", replace(func(p string) error { return os.Mkdir(p, 0o755) }), "通常のファイルではない"},
		{"bundle ではない中身", write("this is not a bundle\n"), "正しくない"},
		{"空のファイル", write(""), "正しくない"},
		{"ヘッダに空行が無い", write("# v2 git bundle\n" + oid + " refs/heads/main\n"), "正しくない"},
		{"ブランチが無い", write("# v2 git bundle\n\nPACK"), "取り込めるブランチ"},
		{"ヘッダが上限 (64 KiB) を超える", write(hugeHeader()), "正しくない"},
		{"安全な名前のブランチが無い", write("# v2 git bundle\n" + oid + " refs/heads/evil;id\n\nPACK"), "取り込めるブランチ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st.afterBundle = tc.hook
			defer func() { st.afterBundle = nil }()
			var b *Bundle
			var err error
			done := make(chan struct{})
			go func() { defer close(done); b, err = st.Export(ctxT(t), sess.ID) }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("Export が止まった (FIFO などを開いて待った)")
			}
			if err == nil {
				t.Fatalf("Export が error にならない: %+v", b)
			}
			if b != nil {
				t.Errorf("error なのに Bundle を返した: %+v", b)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want %q を含む", err, tc.want)
			}
			if exists(filepath.Join(sess.Export, bundleName)) {
				t.Error("拒否した bundle が、export/ に残っている")
			}
		})
	}
	t.Run("大きさの上限を超える", func(t *testing.T) {
		st.maxBundle = 100
		defer func() { st.maxBundle = DefaultMaxBundleBytes }()
		if b, err := st.Export(ctxT(t), sess.ID); err == nil || !strings.Contains(err.Error(), "上限") {
			t.Errorf("Export = %+v, %v, want 上限の error", b, err)
		}
		if exists(filepath.Join(sess.Export, bundleName)) {
			t.Error("上限を超えた bundle が、export/ に残っている")
		}
	})
	t.Run("上限ちょうどは通る", func(t *testing.T) {
		b, err := st.Export(ctxT(t), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		st.maxBundle = b.Size
		defer func() { st.maxBundle = DefaultMaxBundleBytes }()
		if _, err := st.Export(ctxT(t), sess.ID); err != nil {
			t.Errorf("上限ちょうど (%d バイト) の bundle を拒否した: %v", b.Size, err)
		}
	})
}

// TestCreateIDCollisionKeepsOtherSession は、生成した ID が、既にあるセッションと衝突したとき、Create が失敗しても、
// 既にあるセッションを消さないことを確認する。
func TestCreateIDCollisionKeepsOtherSession(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	st, _, _ := testStore(t)
	const id = "20260926-103000-a1b2c3"
	st.newID = func() (string, error) { return id, nil }
	keep := filepath.Join(st.root, id, "clone", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("KEEP"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir}); err == nil {
		t.Fatalf("ID が衝突したのに、Create が error にならない (%+v)", s)
	}
	if got := readFile(t, keep); got != "KEEP" {
		t.Errorf("既にあるセッションのファイルが変わった: %q", got)
	}
}

// TestCreateTimeout は、時間の上限を超えた git を、止めて error にし、セッションを残さないことを確認する。
func TestCreateTimeout(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	st, _, _ := testStore(t)
	st.timeout = time.Millisecond
	if s, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir}); err == nil {
		t.Errorf("Create が error にならない (%+v)", s)
	}
	if ents, _ := os.ReadDir(st.root); len(ents) != 0 {
		t.Errorf("時間切れで失敗したのに、セッションが残った: %v", ents)
	}
}

// TestExportReplacesStaleEntry は、bundle の名前に、前から (檻が) 置いたものがあっても、Export がそれを消して、bundle を作ることを確認する
// (空のディレクトリは、git bundle create が置き換えられない。symlink は、消してから作るので、その先へは書かない)。
func TestExportReplacesStaleEntry(t *testing.T) {
	st, _, sess := exportFixture(t)
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("VICTIM"), 0o644)
	for name, mk := range map[string]func(p string) error{
		"空のディレクトリ":  func(p string) error { return os.Mkdir(p, 0o755) },
		"symlink":   func(p string) error { return os.Symlink(victim, p) },
		"古い bundle": func(p string) error { return os.WriteFile(p, []byte("stale"), 0o644) },
	} {
		p := filepath.Join(sess.Export, bundleName)
		os.Remove(p)
		if err := mk(p); err != nil {
			t.Fatal(err)
		}
		b, err := st.Export(ctxT(t), sess.ID)
		if err != nil || b.Size < 100 {
			t.Errorf("%s: Export = %+v, %v", name, b, err)
		}
		if got := readFile(t, victim); got != "VICTIM" {
			t.Errorf("%s: symlink の先が書き換えられた: %q", name, got)
		}
	}
}

// TestExportUnknownSession は、存在しない・形の正しくない ID を、檻を起動せずに断ることを確認する。
func TestExportUnknownSession(t *testing.T) {
	st, _, _ := testStore(t)
	for _, id := range []string{"20260926-103000-a1b2c3", "", "../x", "x"} {
		if b, err := st.Export(ctxT(t), id); err == nil {
			t.Errorf("Export(%q) = %+v が error にならない", id, b)
		}
	}
}

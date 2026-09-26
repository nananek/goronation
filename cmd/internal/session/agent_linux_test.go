package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// mkSession は、st の下に、ID のセッションのディレクトリを作る (Create を通さない。記録の読み書きだけを確かめるため)。
func mkSession(t *testing.T, st *Store, id string) *Session {
	t.Helper()
	if err := os.Mkdir(filepath.Join(st.root, id), 0o700); err != nil {
		t.Fatal(err)
	}
	sess, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// Store.Agent は、記録が無ければ空 (error ではない: Agent を記録する前のセッション)、正しい記録ならその名前を返す。
// 壊れた記録 (形・大きさ・種類が違う) は、error: 黙って別のエージェントのものとして扱わない。中身は、error に出さない・読まない。
func TestStoreAgent(t *testing.T) {
	st, _, _ := testStore(t)

	sess := mkSession(t, st, "20260926-120000-aaaaaa")
	if got, err := st.Agent(sess); got != "" || err != nil {
		t.Errorf("記録が無い: Agent = %q, %v (want 空・error なし)", got, err)
	}
	for i, tc := range []struct {
		content string
		want    string
	}{
		{"claude\n", "claude"}, {"opencode\n", "opencode"}, {"opencode", "opencode"}, {"a-b-9\n", "a-b-9"}, {strings.Repeat("a", 32) + "\n", strings.Repeat("a", 32)},
	} {
		s := mkSession(t, st, fmt.Sprintf("20260926-%06d-bbbb%02x", 130000+i, i))
		if err := os.WriteFile(filepath.Join(s.Dir, agentFile), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := st.Agent(s); got != tc.want || err != nil {
			t.Errorf("記録 %q: Agent = %q, %v (want %q)", tc.content, got, err, tc.want)
		}
	}

	// 壊れた記録: 形が違う・複数行・空・大きすぎる・制御文字。
	bad := []string{"Claude\n", "claude\nopencode\n", "claude\n\n", "\n", "", " claude\n", "claude \n", "1claude\n", "a b\n", "../x\n",
		strings.Repeat("a", 33) + "\n", strings.Repeat("a", 100) + "\n", "evil\x1b[31m\n", "cl\x00aude\n", "claude\r\n", "エージェント\n"}
	for i, content := range bad {
		s := mkSession(t, st, fmt.Sprintf("20260926-%06d-cccc%02x", 140000+i, i))
		if err := os.WriteFile(filepath.Join(s.Dir, agentFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := st.Agent(s); err == nil || got != "" {
			t.Errorf("壊れた記録 %q: Agent = %q, %v (want error)", content, got, err)
		} else if strings.ContainsAny(err.Error(), "\x1b\x00") {
			t.Errorf("error に、記録の制御文字がそのまま出ている: %q", err)
		}
	}

	// 通常のファイルでない記録 (FIFO・symlink・ディレクトリ): 止まらず、読まず、error。symlink の先の中身は、error に出ない。
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("opencode\n"), 0o600); err != nil { // 形は正しい中身。読まれたら、opencode になってしまう
		t.Fatal(err)
	}
	fifo := mkSession(t, st, "20260926-150000-dddddd")
	if err := syscall.Mkfifo(filepath.Join(fifo.Dir, agentFile), 0o600); err != nil {
		t.Fatal(err)
	}
	link := mkSession(t, st, "20260926-151000-dddddd")
	if err := os.Symlink(secret, filepath.Join(link.Dir, agentFile)); err != nil {
		t.Fatal(err)
	}
	dir := mkSession(t, st, "20260926-152000-dddddd")
	if err := os.Mkdir(filepath.Join(dir.Dir, agentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Session{"FIFO": fifo, "symlink": link, "ディレクトリ": dir} {
		if got, err := st.Agent(s); err == nil || got != "" {
			t.Errorf("%s の記録: Agent = %q, %v (want error。symlink の先を読んではいけない)", name, got, err)
		}
	}
}

// List は、Agent を、記録があればその名前、無ければ空、読めなければ UnknownAgent (?) で返す。
func TestListShowsAgent(t *testing.T) {
	st, _, _ := testStore(t)
	mk := func(id, content string, write bool) {
		s := mkSession(t, st, id)
		if write {
			if err := os.WriteFile(filepath.Join(s.Dir, agentFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("20260926-120000-aaaaaa", "", false) // 古いセッション
	mk("20260926-120001-bbbbbb", "claude\n", true)
	mk("20260926-120002-cccccc", "opencode\n", true)
	mk("20260926-120003-dddddd", "Bad Agent\n", true)
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"20260926-120000-aaaaaa": "", "20260926-120001-bbbbbb": "claude", "20260926-120002-cccccc": "opencode", "20260926-120003-dddddd": UnknownAgent,
	}
	if len(got) != len(want) {
		t.Fatalf("List() = %+v", got)
	}
	for _, in := range got {
		if in.Agent != want[in.ID] {
			t.Errorf("%s の Agent = %q, want %q", in.ID, in.Agent, want[in.ID])
		}
	}
}

// Create は、Agent を、セッションのディレクトリの直下 (檻に bind されない場所) に、0600 で記録する。渡さなければ記録しない。
// 正しくない Agent は、何も作らずに断る。
func TestCreateRecordsAgent(t *testing.T) {
	needBwrap(t)
	src := newSrcRepo(t)
	st, _, _ := testStore(t)

	sess, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir, Agent: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sess.Dir, agentFile)
	if got := readFile(t, path); got != "opencode\n" {
		t.Errorf("記録 = %q, want %q", got, "opencode\n")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("記録の権限 = %v, %v, want 0600", fi, err)
	}
	for _, bound := range []string{sess.Clone, sess.Run, sess.Export} { // 檻に見せる場所の中には、置かない
		if strings.HasPrefix(path, bound+"/") || exists(filepath.Join(bound, agentFile)) {
			t.Errorf("記録が、檻に bind される %s の中にある", bound)
		}
	}
	if got, err := st.Agent(sess); got != "opencode" || err != nil {
		t.Errorf("Agent = %q, %v", got, err)
	}

	plain, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(plain.Dir, agentFile)) {
		t.Error("Agent を渡していないのに、記録が作られた")
	}
	if got, err := st.Agent(plain); got != "" || err != nil {
		t.Errorf("Agent を渡さない: Agent = %q, %v (want 空)", got, err)
	}

	list, err := st.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List() = %+v, %v", list, err)
	}
	for _, in := range list {
		want := map[string]string{sess.ID: "opencode", plain.ID: ""}[in.ID]
		if in.Agent != want {
			t.Errorf("List の %s: Agent = %q, want %q", in.ID, in.Agent, want)
		}
	}

	before, _ := os.ReadDir(st.root)
	for _, bad := range []string{"Claude", "a b", "../x", "1a", strings.Repeat("a", 33), "opencode\n", "エージェント"} {
		if _, err := st.Create(ctxT(t), CreateOptions{Repo: src.Dir, Agent: bad}); err == nil || !strings.Contains(err.Error(), "Agent") {
			t.Errorf("Agent %q: error = %v (want Agent が正しくない)", bad, err)
		}
	}
	if after, _ := os.ReadDir(st.root); len(after) != len(before) {
		t.Errorf("正しくない Agent で断ったのに、セッションのディレクトリが増えた: %d → %d", len(before), len(after))
	}
}

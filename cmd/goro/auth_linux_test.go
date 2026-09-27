package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// authTok は、値の目印 (github の Fine-grained トークンの形)。出力・error のどこにも、現れてはならない。
const authTok = "github_pat_11AAAAAAA0SECRETSECRET_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// leaksAuth は、out に、値 (全体・目印) が含まれていれば、その一部を返す。
func leaksAuth(out string) string {
	for _, part := range []string{authTok, "SECRETSECRET", "github_pat_11", "abcdefghij"} {
		if strings.Contains(out, part) {
			return part
		}
	}
	return ""
}

// pipeStdin は、content を書いて閉じた pipe の読み側 (端末でない標準入力)。
func pipeStdin(t *testing.T, content string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { r.Close() })
	return r
}

// auth は、runAuth を、標準入力 content・状態ディレクトリ state で実行する。
func auth(t *testing.T, state, content string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runAuth(append(args, "--state-dir", state), pipeStdin(t, content), &out, &errb)
	return code, out.String(), errb.String()
}

func TestGitHubTokenForm(t *testing.T) {
	k, ok := credentialKindByName("github")
	if !ok {
		t.Fatal("github が、表に無い")
	}
	pre := "github_pat_"
	for name, v := range map[string]string{
		"本物の形 (93 文字)":      "github_pat_11AAAAAAA0" + strings.Repeat("aB3_", 20) + "xyz",
		"下限 (40 文字)":        pre + strings.Repeat("a", 29),
		"上限 (255 文字)":       pre + strings.Repeat("Z", 244),
		"英数字と _ だけ":         pre + "0123456789_ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz",
		"authTok (テスト用の目印)": authTok,
	} {
		if !k.valid(v) {
			t.Errorf("%s が通らない: %d 文字", name, len(v))
		}
	}
	for name, v := range map[string]string{
		"空": "", "接頭辞だけ": pre, "39 文字": pre + strings.Repeat("a", 28), "256 文字": pre + strings.Repeat("a", 245),
		"classic トークン (ghp_)": "ghp_" + strings.Repeat("a", 36), "接頭辞が違う": "gitlab_pat_" + strings.Repeat("a", 40),
		"大文字の接頭辞": "GITHUB_PAT_" + strings.Repeat("a", 40), "空白を含む": pre + strings.Repeat("a", 20) + " " + strings.Repeat("a", 20),
		"改行を含む": pre + strings.Repeat("a", 40) + "\n", "前に空白": " " + pre + strings.Repeat("a", 40), "- を含む": pre + strings.Repeat("a", 20) + "-" + strings.Repeat("a", 20),
		"非 ASCII": pre + strings.Repeat("a", 40) + "あ", "NUL": pre + strings.Repeat("a", 40) + "\x00", "引用符": pre + strings.Repeat("a", 40) + "'",
	} {
		if k.valid(v) {
			t.Errorf("%s が通った", name)
		}
	}
}

// 表: 資格情報の種類は、名前をキーにした表で、名前は credential の名前の形で、重ならず、説明・検査・表示を持つ。
func TestCredentialKindsTable(t *testing.T) {
	nameRE := regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	seen := map[string]bool{}
	for _, k := range credentialKinds {
		if !nameRE.MatchString(k.name) || seen[k.name] {
			t.Errorf("name %q が、形でない・重なっている", k.name)
		}
		seen[k.name] = true
		if k.valid == nil || k.invalid == "" || k.usage == "" {
			t.Errorf("%s の必須の項目が空", k.name)
		}
		if strings.Count(k.invalid, "\n") != 0 || utf8.RuneCountInString(k.invalid) > 120 {
			t.Errorf("%s の invalid が、1 行 120 文字以内でない: %q", k.name, k.invalid)
		}
		usage := authUsage()
		if !strings.Contains(usage, k.name) || !strings.Contains(usage, strings.SplitN(k.usage, "\n", 2)[0]) {
			t.Errorf("%s の説明が、goro auth -h に出ない:\n%s", k.name, usage)
		}
	}
	if credentialNames() != "github" {
		t.Errorf("credentialNames = %q", credentialNames())
	}
}

// 表に足すだけで、goro auth NAME が通り、保存先が <state>/credentials/<NAME> になる (最初の 1 つを特別扱いしない)。
func TestAuthWorksForAnyRegisteredKind(t *testing.T) {
	saved := credentialKinds
	defer func() { credentialKinds = saved }()
	credentialKinds = append(append([]credentialKind{}, saved...), credentialKind{
		name: "other-cred", valid: func(v string) bool { return strings.HasPrefix(v, "oc_") && len(v) > 8 }, invalid: "形式が違う。oc_ で始めてください。", usage: "テスト用",
	})
	state := t.TempDir()
	code, stdout, stderr := auth(t, state, "oc_abcdefghij\n", "other-cred")
	if code != 0 || !strings.Contains(stdout, filepath.Join(state, "credentials", "other-cred")) {
		t.Fatalf("表に足した名前: 終了コード %d\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	if b, err := os.ReadFile(filepath.Join(state, "credentials", "other-cred")); err != nil || string(b) != "oc_abcdefghij\n" {
		t.Errorf("保存した中身 = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(state, "credentials", "github")); err == nil {
		t.Error("別の名前の保存が、github のファイルを作った")
	}
	if code, _, stderr := auth(t, state, "bad\n", "other-cred"); code != 1 || !strings.Contains(stderr, "oc_ で始めてください") {
		t.Errorf("表に足した検査: %d %s", code, stderr)
	}
	if !strings.Contains(authUsage(), "other-cred") {
		t.Error("goro auth -h に、表に足した名前が出ない")
	}
}

// パイプ (端末でない標準入力) から貼って保存する: 値は、stdout・stderr のどこにも出ず、ファイルは 0600・ディレクトリは 0700。
func TestAuthSavesFromPipe(t *testing.T) {
	for _, input := range []string{authTok + "\n", authTok, "  " + authTok + "  \n", authTok + "\r\n", "\n\n" + authTok + "\n"} {
		state := t.TempDir()
		code, stdout, stderr := auth(t, state, input, "github")
		want := filepath.Join(state, "credentials", "github")
		if strings.HasPrefix(input, "\n") { // 先頭が空行なら、最初の行 (空) を読むので、空の入力
			if code != 1 {
				t.Errorf("先頭が空行 %q: 終了コード %d, want 1", input[:3], code)
			}
			continue
		}
		if code != 0 || stdout != "保存した: "+want+"\n" || stderr != "" {
			t.Errorf("%q: 終了コード %d\nstdout: %q\nstderr: %q", input, code, stdout, stderr)
		}
		if leaksAuth(stdout+stderr) != "" {
			t.Errorf("%q: 出力に値が出た", input)
		}
		if b, err := os.ReadFile(want); err != nil || string(b) != authTok+"\n" {
			t.Errorf("保存した中身が違う (%v)", err)
		}
		if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("ファイルの権限 = %v, %v", fi, err)
		}
		if fi, err := os.Stat(filepath.Dir(want)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("ディレクトリの権限 = %v, %v", fi, err)
		}
	}

	// 上書き。--state-dir は、名前の前でも後ろでも書ける。
	state := t.TempDir()
	auth(t, state, authTok+"\n", "github")
	other := strings.Replace(authTok, "AAAAAAA0", "BBBBBBB1", 1)
	var out, errb bytes.Buffer
	if code := runAuth([]string{"--state-dir", state, "github"}, pipeStdin(t, other+"\n"), &out, &errb); code != 0 {
		t.Fatalf("上書き: %d %s", code, errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(state, "credentials", "github")); string(b) != other+"\n" {
		t.Error("上書きされていない")
	}
}

// 形が違う入力は、1 行の表示で断り、ディスクに何も作らない。値は、どこにも出ない。
func TestAuthRefusesBadTokenWithoutTouchingDisk(t *testing.T) {
	for name, input := range map[string]string{
		"接頭辞が違う": "ghp_" + strings.Repeat("a", 36) + "\n", "短い": "github_pat_short\n", "空白を含む": authTok[:30] + " " + authTok[30:] + "\n",
		"空": "\n", "入力なし": "", "空白だけ": "   \n", "長すぎる": authTok + strings.Repeat("A", 2000) + "\n", "途中に NUL": authTok[:40] + "\x00" + authTok[40:] + "\n",
	} {
		state := t.TempDir()
		code, stdout, stderr := auth(t, state, input, "github")
		if code != 1 || stdout != "" {
			t.Errorf("%s: 終了コード %d, stdout %q (want 1・空)", name, code, stdout)
		}
		if n := strings.Count(strings.TrimSpace(stderr), "\n") + 1; n != 1 || !strings.HasPrefix(stderr, "goro auth: ") {
			t.Errorf("%s: 表示が、goro auth: で始まる 1 行でない: %q", name, stderr)
		}
		if leaksAuth(stdout+stderr) != "" {
			t.Errorf("%s: 出力に値が出た: %q", name, stderr)
		}
		if _, err := os.Lstat(filepath.Join(state, "credentials")); err == nil {
			t.Errorf("%s: 断ったのに、credentials を作った", name)
		}
	}
	// 断り方 (形式) の文言は、次にすること (貼り直し・作り方) を示す。
	_, _, stderr := auth(t, t.TempDir(), "nope\n", "github")
	if !strings.Contains(stderr, "github_pat_ で始まる") || !strings.Contains(stderr, "goro auth -h") {
		t.Errorf("形式の断り: %q", stderr)
	}
}

// 使い方の誤り: 名前が無い・未知・余計な引数・不明なオプションは、終了コード 2 (使える名前を示す)。-h は使い方を出して 0。
func TestAuthUsageErrors(t *testing.T) {
	state := t.TempDir()
	for name, args := range map[string][]string{
		"名前なし": {}, "オプションだけ": {"--state-dir", state}, "未知の名前": {"gitlab"}, "余計な引数": {"github", "extra"}, "名前が形でない": {"../x"},
		"不明なオプション": {"github", "--token", authTok}, "値をオプションで渡す": {"--token=" + authTok},
	} {
		var out, errb bytes.Buffer
		code := runAuth(args, pipeStdin(t, authTok+"\n"), &out, &errb)
		if code != exitUsage {
			t.Errorf("%s: 終了コード %d, want %d\n%s", name, code, exitUsage, errb.String())
		}
		if leaksAuth(out.String()+errb.String()) != "" {
			t.Errorf("%s: 出力に値が出た: %s", name, errb.String())
		}
	}
	if _, err := os.Lstat(filepath.Join(state, "credentials")); err == nil {
		t.Error("使い方の誤りが、credentials を作った")
	}
	// 未知の名前・名前なしは、使える名前を示す。
	var errb bytes.Buffer
	runAuth([]string{"gitlab"}, pipeStdin(t, ""), io.Discard, &errb)
	if !strings.Contains(errb.String(), "使える名前: github") {
		t.Errorf("未知の名前: %q", errb.String())
	}
	// -h。
	errb.Reset()
	if code := runAuth([]string{"-h"}, pipeStdin(t, ""), io.Discard, &errb); code != 0 {
		t.Errorf("-h の終了コード = %d", code)
	}
	for _, want := range []string{"使い方: goro auth NAME", "github", "https://github.com/settings/personal-access-tokens/new", "Contents と Pull requests", "引数には書けない", "表示せずに"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("-h に %q が無い:\n%s", want, errb.String())
		}
	}
}

// 資格情報のディレクトリが安全でない (権限が緩い) ときは、書かず、直すコマンドを出す。値は出ない。
func TestAuthRefusesUnsafeDirectory(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "credentials")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := auth(t, state, authTok+"\n", "github")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "chmod 700 "+dir) || leaksAuth(stderr) != "" {
		t.Errorf("緩いディレクトリ: %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("拒否したのに、何かを書いた: %v", ents)
	}
}

// 端末: 入力を表示しない (ECHO を切る)。値は、端末 (master 側) にも出ない。終わったら、端末の設定が戻る。
func TestAuthHidesInputOnTerminal(t *testing.T) {
	master, slave := openPty(t)
	before := termOf(t, slave)
	if before.Lflag&syscall.ECHO == 0 {
		t.Fatal("前提: pty の ECHO が、最初から切れている")
	}
	state := t.TempDir()
	var out, errb bytes.Buffer
	codeCh := make(chan int, 1)
	go func() { codeCh <- runAuth([]string{"github", "--state-dir", state}, slave, &out, &errb) }()

	deadline := time.Now().Add(5 * time.Second)
	for termOf(t, slave).Lflag&syscall.ECHO != 0 { // 入力を待つ間、ECHO が切れる
		if time.Now().After(deadline) {
			t.Fatal("入力を待つ間、ECHO が切れない")
		}
		time.Sleep(5 * time.Millisecond)
	}
	seen := ""
	if _, err := master.WriteString(authTok + "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("終了コード %d\nstderr: %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("goro auth が終わらない")
	}
	master.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	rest, _ := io.ReadAll(master)
	seen += string(rest)
	if leaksAuth(seen) != "" || leaksAuth(out.String()+errb.String()) != "" {
		t.Errorf("端末に、入力が表示された (エコー) か、出力に値が出た:\n端末: %q\nstdout: %q\nstderr: %q", seen, out.String(), errb.String())
	}
	if after := termOf(t, slave); after != before {
		t.Errorf("終了後、端末の設定が戻っていない:\n before %+v\n after  %+v", before, after)
	}
	if b, err := os.ReadFile(filepath.Join(state, "credentials", "github")); err != nil || string(b) != authTok+"\n" {
		t.Errorf("保存した中身が違う (%v)", err)
	}
	if !strings.Contains(errb.String(), "トークンを貼ってください (表示されません)") { // プロンプトは、stderr (実際は、端末) に出る
		t.Errorf("プロンプトが出ていない: %q", errb.String())
	}
}

// 取り消し (Ctrl-C・SIGTERM 相当): 端末の設定を戻して、待たずに戻る。何も保存しない。
func TestReadSecretLineCancelRestoresTerminal(t *testing.T) {
	_, slave := openPty(t)
	before := termOf(t, slave)
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	res := make(chan error, 1)
	go func() {
		_, err := readSecretLine(ctx, slave, "p: ", &out)
		res <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for termOf(t, slave).Lflag&syscall.ECHO != 0 { // ECHO が切れる (= 入力待ちに入る) のを待つ
		if time.Now().After(deadline) {
			t.Fatal("ECHO が切れない")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("取り消しの error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取り消しても、戻らない")
	}
	if after := termOf(t, slave); after != before {
		t.Errorf("取り消し後、端末の設定が戻っていない:\n before %+v\n after  %+v", before, after)
	}
}

// 端末でない入力の読み: 1 行だけ読み (残りは読まない)・改行の無い最後の行・空・上限を超える行。
func TestReadSecretLine(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		in   string
		want string
		err  bool
	}{
		"改行つき": {"abc\n", "abc", false}, "改行なし": {"abc", "abc", false}, "2 行": {"abc\ndef\n", "abc", false}, "空行": {"\nabc", "", false},
		"入力なし": {"", "", true}, "CR は残す": {"abc\r\n", "abc\r", false},
	} {
		got, err := readSecretLine(ctx, pipeStdin(t, tc.in), "", io.Discard)
		if (err != nil) != tc.err || string(got) != tc.want {
			t.Errorf("%s: %q, %v, want %q・error=%v", name, got, err, tc.want, tc.err)
		}
	}
	long, err := readSecretLine(ctx, pipeStdin(t, strings.Repeat("A", 5000)+"\n"), "", io.Discard)
	if err != nil || len(long) > maxSecretLine+1 {
		t.Errorf("長すぎる行: %d バイト, %v (上限 %d+1 まで)", len(long), err, maxSecretLine)
	}
}

// 値が、error・表示・%v・panic に出ないこと: goro auth の全ての失敗経路で、stdout・stderr に値が無い。
func TestAuthNeverPrintsTheToken(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "credentials")
	os.MkdirAll(dir, 0o700)
	os.Symlink("/nonexistent", filepath.Join(dir, "github")) // 置き換え自体は成功する (symlink を置き換える)
	for name, run := range map[string]func() (int, string, string){
		"成功":            func() (int, string, string) { return auth(t, t.TempDir(), authTok+"\n", "github") },
		"symlink を置き換え": func() (int, string, string) { return auth(t, state, authTok+"\n", "github") },
		"状態ディレクトリが相対": func() (int, string, string) {
			var o, e bytes.Buffer
			c := runAuth([]string{"github", "--state-dir", "rel\x00ative"}, pipeStdin(t, authTok+"\n"), &o, &e)
			return c, o.String(), e.String()
		},
		"状態ディレクトリがファイル": func() (int, string, string) {
			f := filepath.Join(t.TempDir(), "file")
			os.WriteFile(f, []byte("x"), 0o600)
			return auth(t, f, authTok+"\n", "github")
		},
		"形が違う": func() (int, string, string) { return auth(t, t.TempDir(), authTok[:20]+"\n", "github") },
	} {
		code, stdout, stderr := run()
		if leaksAuth(stdout+stderr) != "" {
			t.Errorf("%s: 終了コード %d の出力に値が出た:\nstdout: %q\nstderr: %q", name, code, stdout, stderr)
		}
	}
}

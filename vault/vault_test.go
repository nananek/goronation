package vault

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nananek/goronation/core/credential"
	corevault "github.com/nananek/goronation/core/vault"
)

func TestLifecycleInitCloseOpenUnlock(t *testing.T) {
	v, dir := initVault(t)
	if !v.Initialized() || !v.Unlocked() {
		t.Fatal("Init の後は、初期化済みで解錠中")
	}
	if err := v.Init("cred-a", fakeSalt(t), fakePRF("a")); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("2 回目の Init = %v, want ErrAlreadyInitialized", err)
	}
	v.Lock()
	if v.Unlocked() {
		t.Fatal("Lock の後は施錠中")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock("cred-a", fakePRF("a")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 後の Unlock = %v, want ErrClosed", err)
	}

	v2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	if !v2.Initialized() || v2.Unlocked() {
		t.Fatal("開き直した直後は、初期化済みで施錠中")
	}
	if err := v2.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatalf("Unlock = %v", err)
	}
	if !v2.Unlocked() {
		t.Fatal("Unlock の後は解錠中")
	}
	if infos := v2.WrapInfos(); len(infos) != 1 || infos[0].CredentialID != "cred-a" || len(infos[0].Salt) != SaltSize {
		t.Fatalf("WrapInfos = %+v", infos)
	}
}

func TestUninitialized(t *testing.T) {
	v, err := Open(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.Initialized() || v.Unlocked() || v.WrapInfos() != nil {
		t.Fatal("新しい Vault は、未初期化")
	}
	if err := v.Unlock("cred-a", fakePRF("a")); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Unlock = %v", err)
	}
	if err := v.PutCredential("github", credential.New("x")); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("PutCredential = %v", err)
	}
	if _, err := v.Token(context.Background(), "github"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Token = %v", err)
	}
	p := Proof{CredentialID: "cred-a", PRF: fakePRF("a")}
	if err := v.AddWrap(p, Enrollment{CredentialID: "cred-b", Salt: fakeSalt(t), PRF: fakePRF("b")}); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("AddWrap = %v", err)
	}
	if err := v.RemoveWrap(p, "cred-b"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("RemoveWrap = %v", err)
	}
}

func TestInitInputValidation(t *testing.T) {
	v, err := Open(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	salt := fakeSalt(t)
	bad := []struct {
		name string
		id   string
		salt []byte
		prf  []byte
	}{
		{"empty id", "", salt, fakePRF("a")},
		{"id charset", "a b", salt, fakePRF("a")},
		{"id newline", "a\nb", salt, fakePRF("a")},
		{"id too long", string(make([]byte, maxIDLen+1)), salt, fakePRF("a")},
		{"short salt", "a", salt[:31], fakePRF("a")},
		{"long salt", "a", append(salt, 0), fakePRF("a")},
		{"short prf", "a", salt, fakePRF("a")[:31]},
		{"nil prf", "a", salt, nil},
	}
	for _, c := range bad {
		if err := v.Init(c.id, c.salt, c.prf); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: Init = %v, want ErrInvalidInput", c.name, err)
		}
	}
	if v.Initialized() {
		t.Fatal("不正な入力で、初期化されない")
	}
}

func TestUnlockFailuresAreGeneric(t *testing.T) {
	v, _ := initVault(t)
	v.Lock()
	for name, c := range map[string]struct {
		id  string
		prf []byte
	}{
		"wrong prf":  {"cred-a", fakePRF("zzz")},
		"unknown id": {"cred-zzz", fakePRF("a")},
	} {
		if err := v.Unlock(c.id, c.prf); !errors.Is(err, ErrUnlockFailed) {
			t.Errorf("%s: Unlock = %v, want ErrUnlockFailed", name, err)
		}
	}
	if v.Unlocked() {
		t.Fatal("失敗した Unlock で、解錠されない")
	}
	if err := v.Unlock("cred-a", fakePRF("a")[:16]); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("短い PRF = %v, want ErrInvalidInput", err)
	}
}

func TestAddWrapAndUnlockWithNewPasskey(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	if infos := v.WrapInfos(); len(infos) != 2 {
		t.Fatalf("WrapInfos = %+v", infos)
	}
	// 追加は、本体鍵と保管した内容を、作り直さない。
	if err := v.PutCredential("github", credential.New("tok-1")); err != nil {
		t.Fatal(err)
	}
	v.Close()

	v2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	if err := v2.Unlock("cred-b", fakePRF("b")); err != nil {
		t.Fatalf("追加した passkey で Unlock = %v", err)
	}
	tok, err := v2.Token(context.Background(), "github")
	if err != nil || tok.Reveal() != "tok-1" {
		t.Fatalf("Token = %v, %v", tok, err)
	}
}

func TestAddWrapWorksWhileLocked(t *testing.T) {
	v, _ := initVault(t)
	v.Lock()
	addPasskey(t, v, "cred-b", "b") // 認可は authorizer の証明だけ。解錠中かは見ない。
	if v.Unlocked() {
		t.Fatal("AddWrap は、解錠しない")
	}
	if err := v.Unlock("cred-b", fakePRF("b")); err != nil {
		t.Fatalf("Unlock = %v", err)
	}
}

func TestAddWrapErrors(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	before := snapshot(t, dir)
	good := Proof{CredentialID: "cred-a", PRF: fakePRF("a")}
	enrol := func(id string) Enrollment { return Enrollment{CredentialID: id, Salt: fakeSalt(t), PRF: fakePRF("c")} }
	cases := []struct {
		name string
		auth Proof
		cand Enrollment
		want error
	}{
		{"exists (authorizer 自身)", good, enrol("cred-a"), ErrWrapExists},
		{"exists (別の passkey)", good, enrol("cred-b"), ErrWrapExists},
		{"bad authorizer prf", Proof{CredentialID: "cred-a", PRF: fakePRF("x")}, enrol("cred-c"), ErrUnlockFailed},
		{"unknown authorizer", Proof{CredentialID: "cred-x", PRF: fakePRF("a")}, enrol("cred-c"), ErrUnlockFailed},
		{"authorizer の PRF が別の passkey の値", Proof{CredentialID: "cred-a", PRF: fakePRF("b")}, enrol("cred-c"), ErrUnlockFailed},
		{"short candidate salt", good, Enrollment{CredentialID: "cred-c", Salt: []byte{1}, PRF: fakePRF("c")}, ErrInvalidInput},
		{"short candidate prf", good, Enrollment{CredentialID: "cred-c", Salt: fakeSalt(t), PRF: []byte{1}}, ErrInvalidInput},
		{"bad candidate id", good, enrol("c c"), ErrInvalidInput},
	}
	for _, c := range cases {
		if err := v.AddWrap(c.auth, c.cand); !errors.Is(err, c.want) {
			t.Errorf("%s: AddWrap = %v, want %v", c.name, err, c.want)
		}
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
}

func TestAddWrapCap(t *testing.T) {
	v, _ := initVault(t)
	v.mu.Lock() // 15 回の fsync を避けて、上限ちょうどのラップを、メモリに直接置く
	for len(v.doc.Wraps) < maxWraps {
		w := v.doc.Wraps[0]
		w.CredentialID = "cred-fill-" + string(rune('a'+len(v.doc.Wraps)))
		v.doc.Wraps = append(v.doc.Wraps, w)
	}
	v.mu.Unlock()
	err := v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, Enrollment{CredentialID: "cred-over", Salt: fakeSalt(t), PRF: fakePRF("o")})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("上限を超える追加 = %v", err)
	}
}

func TestRemoveWrap(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	a := Proof{CredentialID: "cred-a", PRF: fakePRF("a")}
	b := Proof{CredentialID: "cred-b", PRF: fakePRF("b")}

	before := snapshot(t, dir)
	for name, tc := range map[string]struct {
		auth   Proof
		target string
		want   error
	}{
		"same credential":    {a, "cred-a", ErrSameCredential},
		"unknown target":     {a, "cred-zzz", ErrWrapNotFound},
		"bad authorizer":     {Proof{CredentialID: "cred-a", PRF: fakePRF("x")}, "cred-b", ErrUnlockFailed},
		"unknown authorizer": {Proof{CredentialID: "cred-x", PRF: fakePRF("a")}, "cred-b", ErrUnlockFailed},
		"bad target id":      {a, "b b", ErrInvalidInput},
	} {
		if err := v.RemoveWrap(tc.auth, tc.target); !errors.Is(err, tc.want) {
			t.Errorf("%s: RemoveWrap = %v, want %v", name, err, tc.want)
		}
	}
	mustEqual(t, "vault.json (失敗の後)", snapshot(t, dir), before)

	v.Lock()
	if err := v.RemoveWrap(a, "cred-b"); !errors.Is(err, ErrLocked) {
		t.Fatalf("施錠中の RemoveWrap = %v, want ErrLocked", err)
	}
	mustEqual(t, "vault.json (施錠中)", snapshot(t, dir), before)
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatal(err)
	}

	if err := v.RemoveWrap(a, "cred-b"); err != nil {
		t.Fatalf("RemoveWrap = %v", err)
	}
	if infos := v.WrapInfos(); len(infos) != 1 || infos[0].CredentialID != "cred-a" {
		t.Fatalf("WrapInfos = %+v", infos)
	}
	v.Lock()
	if err := v.Unlock("cred-b", fakePRF("b")); !errors.Is(err, ErrUnlockFailed) {
		t.Fatalf("削除した passkey で Unlock できてはいけない: %v", err)
	}
	// 最後の 1 つは、消せない (別の passkey での認可は無いので、b を使う経路は、もう無い)。
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	if err := v.RemoveWrap(b, "cred-a"); !errors.Is(err, ErrUnlockFailed) {
		t.Fatalf("削除済みの passkey は authorizer になれない: %v", err)
	}
}

func TestRemoveLastWrapRefused(t *testing.T) {
	v, dir := initVault(t)
	before := snapshot(t, dir)
	a := Proof{CredentialID: "cred-a", PRF: fakePRF("a")}
	// ラップが 1 つだけの Vault では、別の passkey の証明は作れない。自分自身を消す要求は ErrSameCredential、別の ID を消す要求は
	// ErrLastWrap で断る (どちらも、ラップは消えない)。
	if err := v.RemoveWrap(a, "cred-a"); !errors.Is(err, ErrSameCredential) {
		t.Fatalf("RemoveWrap(自分自身) = %v", err)
	}
	if err := v.RemoveWrap(a, "cred-b"); !errors.Is(err, ErrLastWrap) {
		t.Fatalf("RemoveWrap(最後の 1 つ) = %v, want ErrLastWrap", err)
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatalf("最後のラップは、残っている: %v", err)
	}
}

func TestCredentialStore(t *testing.T) {
	v, dir := initVault(t)
	ctx := context.Background()
	if _, err := v.Token(ctx, "github"); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("Token(無い) = %v", err)
	}
	if err := v.PutCredential("github", credential.New("tok-1")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	if err := v.PutCredential("github", credential.New("tok-1")); err != nil { // 書き込みごとに、鍵と暗号文が変わる
		t.Fatal(err)
	}
	if string(before) == string(snapshot(t, dir)) {
		t.Fatal("同じ値の書き直しでも、暗号文が変わる")
	}
	if err := v.PutCredential("github", credential.New("tok-2")); err != nil {
		t.Fatal(err)
	}
	tok, err := v.Token(ctx, "github")
	if err != nil || tok.Reveal() != "tok-2" {
		t.Fatalf("Token = %v, %v", tok, err)
	}
	for _, bad := range []string{"", "Bad Name", "../x", "a\x00b"} {
		if err := v.PutCredential(bad, credential.New("x")); !errors.Is(err, credential.ErrInvalidName) {
			t.Errorf("PutCredential(%q) = %v, want ErrInvalidName", bad, err)
		}
		if _, err := v.Token(ctx, bad); !errors.Is(err, credential.ErrInvalidName) {
			t.Errorf("Token(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	if err := v.PutCredential("empty", credential.New("")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("空の値 = %v", err)
	}
	if err := v.PutCredential("big", credential.New(string(make([]byte, maxSecretSize+1)))); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("大きすぎる値 = %v", err)
	}
	v.Lock()
	if _, err := v.Token(ctx, "github"); !errors.Is(err, ErrLocked) || !errors.Is(err, corevault.ErrLocked) {
		t.Fatalf("施錠中の Token = %v, want ErrLocked", err)
	}
	if err := v.PutCredential("github", credential.New("x")); !errors.Is(err, ErrLocked) {
		t.Fatalf("施錠中の PutCredential = %v", err)
	}
}

func TestItemCap(t *testing.T) {
	v, _ := initVault(t)
	if err := v.PutCredential("first", credential.New("x")); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock() // 上限ちょうどまで、項目をメモリに直接置く (fsync を避ける)
	for len(v.doc.Items) < maxItems {
		it := v.doc.Items[0]
		it.Name = "fill-" + string(rune('a'+len(v.doc.Items)/26)) + string(rune('a'+len(v.doc.Items)%26))
		v.doc.Items = append(v.doc.Items, it)
	}
	v.mu.Unlock()
	if err := v.PutCredential("overflow", credential.New("x")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("上限を超える項目 = %v", err)
	}
	if err := v.PutCredential("first", credential.New("y")); err != nil { // 置き換えは、数を増やさない
		t.Fatalf("置き換え = %v", err)
	}
}

func TestLoginStore(t *testing.T) {
	v, dir := initVault(t)
	ctx := context.Background()
	if _, err := v.Get(ctx, "claude"); !errors.Is(err, corevault.ErrNotFound) {
		t.Fatalf("Get(無い) = %v", err)
	}
	files := []corevault.StateFile{{Name: ".credentials.json", Data: []byte(`{"a":1}`)}, {Name: "sub/x.json", Data: []byte("y")}}
	if err := v.Put(ctx, "claude", files); err != nil {
		t.Fatal(err)
	}
	got, err := v.Get(ctx, "claude")
	if err != nil || len(got) != 2 || got[0].Name != ".credentials.json" || string(got[0].Data) != `{"a":1}` || got[1].Name != "sub/x.json" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	// 上限・名前の違反は、保管せず、エラー (Put)。
	before := snapshot(t, dir)
	for name, bad := range map[string][]corevault.StateFile{
		"abs":    {{Name: "/etc/x"}},
		"dotdot": {{Name: "../x"}},
		"big":    {{Name: "a", Data: make([]byte, corevault.MaxStateFileSize+1)}},
		"many":   make([]corevault.StateFile, corevault.MaxStateFiles+1),
	} {
		if err := v.Put(ctx, "claude", bad); !errors.Is(err, corevault.ErrInvalidState) {
			t.Errorf("%s: Put = %v, want ErrInvalidState", name, err)
		}
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
	for _, bad := range []string{"", "Bad", "a/b"} {
		if err := v.Put(ctx, bad, files); !errors.Is(err, credential.ErrInvalidName) {
			t.Errorf("Put(agent=%q) = %v", bad, err)
		}
	}
	v.Lock()
	if _, err := v.Get(ctx, "claude"); !errors.Is(err, corevault.ErrLocked) {
		t.Fatalf("施錠中の Get = %v", err)
	}
	if err := v.Put(ctx, "claude", files); !errors.Is(err, corevault.ErrLocked) {
		t.Fatalf("施錠中の Put = %v", err)
	}
}

// Get (復元) も、上限を検査する: 上限を超える状態を、直接ディスクに書かれても (Put を通らずに)、返さない。
func TestLoginStoreGetChecksLimits(t *testing.T) {
	v, _ := initVault(t)
	ctx := context.Background()
	huge := []corevault.StateFile{{Name: "../evil", Data: []byte("x")}}
	pt := []byte(`{"files":[{"name":"../evil","data":"eA=="}]}`)
	_ = huge
	if err := v.putItem(kindLoginState, "claude", pt); err != nil { // Put の検査を通さずに、暗号化して置く
		t.Fatal(err)
	}
	if _, err := v.Get(ctx, "claude"); !errors.Is(err, corevault.ErrInvalidState) {
		t.Fatalf("Get = %v, want ErrInvalidState", err)
	}
}

func TestOpenIsExclusive(t *testing.T) {
	_, dir := initVault(t)
	if _, err := Open(dir); !errors.Is(err, ErrInUse) {
		t.Fatalf("2 つ目の Open = %v, want ErrInUse", err)
	}
}

func TestOpenAfterCloseReleasesLock(t *testing.T) {
	v, dir := initVault(t)
	v.Close()
	v2, err := Open(dir)
	if err != nil {
		t.Fatalf("Close の後の Open = %v", err)
	}
	v2.Close()
}

func TestWrapInfosAreCopies(t *testing.T) {
	v, _ := initVault(t)
	infos := v.WrapInfos()
	want := slices.Clone(infos[0].Salt)
	infos[0].Salt[0] ^= 0xff
	if !slices.Equal(v.WrapInfos()[0].Salt, want) {
		t.Fatal("WrapInfos の salt を書き換えても、Vault の中身は変わらない")
	}
}

func TestPathTraversalNameCannotEscape(t *testing.T) {
	v, dir := initVault(t)
	_ = v
	if err := v.PutCredential("../evil", credential.New("x")); err == nil {
		t.Fatal("名前は path に使わないが、形を満たさない名前は断る")
	}
	entries, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), "*"))
	if len(entries) != 1 {
		t.Fatalf("dir の外にファイルができた: %v", entries)
	}
}

// TestBodyKeyIsClearedOnLockCloseAndReplace は、本体鍵の slice が、Lock・Close・Unlock による置き換えで、ゼロになることを確かめる
// (メモリに、古い鍵を残さない)。
func TestBodyKeyIsClearedOnLockCloseAndReplace(t *testing.T) {
	zero := func(b []byte) bool { return bytes.Equal(b, make([]byte, len(b))) }
	v, _ := initVault(t)
	k := v.key
	if len(k) != keySize || zero(k) {
		t.Fatalf("解錠中の本体鍵が不正: %x", k)
	}
	v.Lock()
	if !zero(k) || v.key != nil {
		t.Fatal("Lock の後に、本体鍵が残っている")
	}
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	k = v.key
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	if !zero(k) {
		t.Fatal("Unlock が、置き換えた古い本体鍵を消していない")
	}
	k = v.key
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if !zero(k) || v.key != nil {
		t.Fatal("Close の後に、本体鍵が残っている")
	}
}

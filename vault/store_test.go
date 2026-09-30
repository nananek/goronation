package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/goronation/core/credential"
)

func TestOpenCreatesPrivateDir(t *testing.T) {
	dir := newDir(t)
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir = %v, %v (0700 で作る)", fi, err)
	}
}

func TestOpenRejectsUnsafeDir(t *testing.T) {
	base := t.TempDir()
	wide := filepath.Join(base, "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(wide); err == nil {
		t.Error("権限が広い dir を、開いてはいけない")
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Error("symlink の dir を、開いてはいけない")
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Error("通常のファイルを、dir として開いてはいけない")
	}
	if _, err := Open(filepath.Join(base, "no", "such", "parent")); err == nil {
		t.Error("親が無い path は、作らない")
	}
}

func TestFilesArePrivate(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	for _, name := range []string{fileName, auditName, lockName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s = %v, %v (0600)", name, fi, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, tmpName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("一時ファイルが残っている: %v", err)
	}
}

func TestOpenRejectsTamperedFiles(t *testing.T) {
	// 元になる vault.json は、1 回だけ作る (fsync が遅いので、subtest ごとには作らない)。
	base := func() []byte {
		v, dir := initVault(t)
		addPasskey(t, v, "cred-b", "b")
		if err := v.PutCredential("github", credential.New("x")); err != nil {
			t.Fatal(err)
		}
		v.Close()
		return snapshot(t, dir)
	}()
	good := func(t *testing.T) (string, document) {
		dir := newDir(t)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fileName), base, 0o600); err != nil {
			t.Fatal(err)
		}
		var d document
		if err := json.Unmarshal(base, &d); err != nil {
			t.Fatal(err)
		}
		return dir, d
	}
	write := func(t *testing.T, dir string, d document) {
		b, _ := json.Marshal(d)
		if err := os.WriteFile(filepath.Join(dir, fileName), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mut := map[string]func(d *document){
		"version":   func(d *document) { d.Version = 2 },
		"version 0": func(d *document) { d.Version = 0 },
		"short id":  func(d *document) { d.ID = d.ID[:8] },
		"no wraps":  func(d *document) { d.Wraps = nil },
		"too many wraps": func(d *document) {
			for len(d.Wraps) <= maxWraps {
				d.Wraps = append(d.Wraps, d.Wraps[0])
			}
		},
		"dup wrap":      func(d *document) { d.Wraps = append(d.Wraps, d.Wraps[0]) },
		"bad wrap id":   func(d *document) { d.Wraps[0].CredentialID = "a b" },
		"short salt":    func(d *document) { d.Wraps[0].Salt = d.Wraps[0].Salt[:3] },
		"short nonce":   func(d *document) { d.Wraps[0].Nonce = nil },
		"empty ct":      func(d *document) { d.Wraps[0].Ct = nil },
		"bad kind":      func(d *document) { d.Items[0].Kind = "evil" },
		"bad item name": func(d *document) { d.Items[0].Name = "../x" },
		"short rand":    func(d *document) { d.Items[0].Rand = d.Items[0].Rand[:2] },
		"dup item":      func(d *document) { d.Items = append(d.Items, d.Items[0]) },
		"too many items": func(d *document) {
			for len(d.Items) <= maxItems {
				d.Items = append(d.Items, d.Items[0])
				d.Items[len(d.Items)-1].Name = "x" + string(rune('a'+len(d.Items)%26)) + string(rune('a'+len(d.Items)/26%26)) + string(rune('a'+len(d.Items)/676))
			}
		},
	}
	for name, f := range mut {
		t.Run(name, func(t *testing.T) {
			dir, d := good(t)
			f(&d)
			write(t, dir, d)
			if _, err := Open(dir); !errors.Is(err, ErrFormat) {
				t.Fatalf("Open = %v, want ErrFormat", err)
			}
		})
	}
	raw := map[string]func(b []byte) []byte{
		"unknown field": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`{"version"`), []byte(`{"extra":1,"version"`), 1)
		},
		"trailing": func(b []byte) []byte { return append(b, []byte(` {}`)...) },
		"garbage":  func([]byte) []byte { return []byte("\x00\x01") },
		"empty":    func([]byte) []byte { return nil },
		"too big":  func([]byte) []byte { return bytes.Repeat([]byte(" "), maxFileSize+1) },
	}
	for name, f := range raw {
		t.Run(name, func(t *testing.T) {
			dir, _ := good(t)
			b := f(snapshot(t, dir))
			if err := os.WriteFile(filepath.Join(dir, fileName), b, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); !errors.Is(err, ErrFormat) {
				t.Fatalf("Open = %v, want ErrFormat", err)
			}
		})
	}
	t.Run("wide permissions", func(t *testing.T) {
		dir, _ := good(t)
		if err := os.Chmod(filepath.Join(dir, fileName), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); !errors.Is(err, ErrFormat) {
			t.Fatalf("Open = %v, want ErrFormat", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir, _ := good(t)
		other := filepath.Join(t.TempDir(), "x.json")
		if err := os.WriteFile(other, snapshot(t, dir), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, fileName))
		if err := os.Symlink(other, filepath.Join(dir, fileName)); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); !errors.Is(err, ErrFormat) {
			t.Fatalf("Open = %v, want ErrFormat", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir, _ := good(t)
		os.Remove(filepath.Join(dir, fileName))
		if err := os.Mkdir(filepath.Join(dir, fileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); !errors.Is(err, ErrFormat) {
			t.Fatalf("Open = %v, want ErrFormat", err)
		}
	})
}

// 別の Vault の項目・ラップを、そのまま貼っても、復号できない (AAD が、Vault の ID・種類・名前を束縛する)。
func TestSwappedRecordsFailToOpen(t *testing.T) {
	v1, dir1 := initVault(t)
	if err := v1.PutCredential("github", credential.New("tok")); err != nil {
		t.Fatal(err)
	}
	if err := v1.PutCredential("gitlab", credential.New("tok2")); err != nil {
		t.Fatal(err)
	}
	v1.Close()
	var d document
	if err := json.Unmarshal(snapshot(t, dir1), &d); err != nil {
		t.Fatal(err)
	}
	// 項目の名前を入れ替える (暗号文は、そのまま)。
	d.Items[0].Name, d.Items[1].Name = d.Items[1].Name, d.Items[0].Name
	b, _ := json.Marshal(d)
	if err := os.WriteFile(filepath.Join(dir1, fileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Open(dir1)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"github", "gitlab"} {
		if _, err := v.Token(context.Background(), n); !errors.Is(err, ErrFormat) {
			t.Errorf("Token(%s) = %v, want ErrFormat (入れ替えた項目は、復号できない)", n, err)
		}
	}
}

func TestLeftoverTmpFileIsReplaced(t *testing.T) {
	v, dir := initVault(t)
	if err := os.WriteFile(filepath.Join(dir, tmpName), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	addPasskey(t, v, "cred-b", "b")
	if _, err := os.Stat(filepath.Join(dir, tmpName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("一時ファイルが残っている: %v", err)
	}
}

func TestFailedWriteKeepsMemoryAndDisk(t *testing.T) {
	v, dir := initVault(t)
	before := snapshot(t, dir)
	// 一時ファイルの場所を、ディレクトリにして、書けなくする (root でも、書けない)。
	if err := os.Mkdir(filepath.Join(dir, tmpName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmpName, "x"), nil, 0o600); err != nil { // Remove で消せないように、中身を置く
		t.Fatal(err)
	}
	err := v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, Enrollment{CredentialID: "cred-b", Salt: fakeSalt(t), PRF: fakePRF("b")})
	if err == nil {
		t.Fatal("書けないのに、成功した")
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
	if got := v.WrapInfos(); len(got) != 1 {
		t.Fatalf("メモリの状態が変わった: %+v", got)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("error に秘密を含めない")
	}
}

func FuzzLoadDocument(f *testing.F) {
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`{"version":1,"id":"AAAAAAAAAAAAAAAAAAAAAA==","wraps":[{"credential_id":"a","salt":"","nonce":"","ct":""}],"items":[]}`))
	f.Add([]byte(""))
	f.Add([]byte("null"))
	f.Add([]byte("[]"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := filepath.Join(t.TempDir(), "v")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Skip()
		}
		if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o600); err != nil {
			t.Skip()
		}
		d, err := loadDocument(dir)
		if err != nil {
			if !errors.Is(err, ErrFormat) {
				t.Fatalf("error は ErrFormat を包む: %v", err)
			}
			return
		}
		if d != nil && d.validate() != nil {
			t.Fatal("受理した document は、検証を通る")
		}
	})
}

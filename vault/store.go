package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nananek/goronation/core/credential"
)

// ファイル名と上限。
const (
	fileName    = "vault.json"
	tmpName     = "vault.json.tmp"
	lockName    = "lock"
	auditName   = "audit.log"
	maxFileSize = 32 << 20 // vault.json を読む上限
	maxWraps    = 16
	maxItems    = 128

	// formatVersion は、ディスク上の形式の版。合わない Vault は、解錠しない (削除と reset だけ)。
	formatVersion = 1
)

// ErrFormat は、vault.json が読めない・形式の版が合わないときの error (errors.Is で判定する)。
var ErrFormat = errors.New("vault: unsupported or corrupt format")

// ErrInvalidInput は、呼び手が渡した値 (credential ID・PRF・salt・名前・大きさ) が、規則に合わないときの error。
var ErrInvalidInput = errors.New("vault: invalid input")

// maxIDLen は、credential ID (base64url の文字列) の長さの上限。WebAuthn の credential ID は最大 1023 バイトで、base64url で 1364 文字。
const maxIDLen = 1400

// validID は、s が credential ID の形 (base64url の文字だけ・1〜1400 文字) か。ディスクにも記録にも書くので、形を限る。
func validID(s string) bool {
	if s == "" || len(s) > maxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// wrapRecord は、passkey 1 つ分のラップ。salt は、PRF に渡す入力で、秘密ではない。
type wrapRecord struct {
	CredentialID string `json:"credential_id"`
	Salt         []byte `json:"salt"`
	Nonce        []byte `json:"nonce"`
	Ct           []byte `json:"ct"`
}

// itemRecord は、暗号化した項目 1 つ。
type itemRecord struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Rand []byte `json:"rand"`
	Ct   []byte `json:"ct"`
}

// document は、vault.json の中身。
type document struct {
	Version int          `json:"version"`
	ID      []byte       `json:"id"`
	Wraps   []wrapRecord `json:"wraps"`
	Items   []itemRecord `json:"items"`
}

// checkDir は、dir が、自分 (euid) の持つ、自分だけが入れる (0700 相当の) 実ディレクトリか確かめる。無ければ作る (親は、あること)。
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("vault: ディレクトリを作れない: %w", err)
		}
		if fi, err = os.Lstat(dir); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("vault: %s は実ディレクトリでない (symlink・ファイルは使えない)", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("vault: %s の権限 %o が広い (0700 にする)", dir, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("vault: %s の所有者が、自分 (euid) でない", dir)
	}
	return nil
}

// openFlags は、symlink を辿らない開き方。
const noFollow = syscall.O_NOFOLLOW

// lockDir は、dir の lock ファイルの flock (排他・待たない) を取る。取れなければ ErrInUse。返す File を閉じると、解ける
// (kernel は、プロセスが死んでも解く)。
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE|noFollow, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vault: lock ファイルを開けない: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("vault: flock: %w", err)
	}
	return f, nil
}

// readRegular は、dir の下の name を、通常のファイル・自分の所有・0600 相当・上限以内であることを確かめて読む。
// 無ければ (nil, os.ErrNotExist を包んだ error)。
func readRegular(dir, name string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|noFollow|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("vault: %s は通常のファイルでない", name)
	case fi.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("vault: %s の権限 %o が広い (0600 にする)", name, fi.Mode().Perm())
	case !ok || int(st.Uid) != os.Geteuid():
		return nil, fmt.Errorf("vault: %s の所有者が、自分 (euid) でない", name)
	case fi.Size() > limit:
		return nil, fmt.Errorf("vault: %s が大きすぎる", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("vault: %s が大きすぎる", name)
	}
	return data, nil
}

// loadDocument は、vault.json を読んで検証する。無ければ (nil, nil)。読めない・版が違う・上限を超える・形が不正なら、ErrFormat を包む。
func loadDocument(dir string) (*document, error) {
	data, err := readRegular(dir, fileName, maxFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d document
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%w: JSON を読めない", ErrFormat)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: JSON の後ろにデータがある", ErrFormat)
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// validate は、document の形と上限を確かめる (ディスクの内容は、信用しない)。
func (d *document) validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrFormat, msg) }
	if d.Version != formatVersion {
		return bad(fmt.Sprintf("形式の版 %d は扱えない", d.Version))
	}
	if len(d.ID) != vaultIDSize {
		return bad("ID の大きさが不正")
	}
	if len(d.Wraps) == 0 || len(d.Wraps) > maxWraps {
		return bad("ラップの数が不正")
	}
	if len(d.Items) > maxItems {
		return bad("項目の数が不正")
	}
	seen := map[string]struct{}{}
	for _, w := range d.Wraps {
		if !validID(w.CredentialID) || len(w.Salt) != SaltSize || len(w.Nonce) != nonceSize || len(w.Ct) == 0 {
			return bad("ラップが不正")
		}
		if _, dup := seen[w.CredentialID]; dup {
			return bad("ラップの credential ID が重複している")
		}
		seen[w.CredentialID] = struct{}{}
	}
	seenItem := map[string]struct{}{}
	for _, it := range d.Items {
		if !validKind(it.Kind) || credential.CheckName(it.Name) != nil || len(it.Rand) != itemRndSize || len(it.Ct) == 0 {
			return bad("項目が不正")
		}
		key := it.Kind + "\x00" + it.Name
		if _, dup := seenItem[key]; dup {
			return bad("項目が重複している")
		}
		seenItem[key] = struct{}{}
	}
	return nil
}

// ErrNotDurable は、vault.json を新しい内容に置き換えた (rename 済み) が、ディレクトリの fsync に失敗したときの error。
// 変更は適用済みで、メモリも新しい内容に合わせてある (永続化が未確認なだけ)。errors.Is で判定する。
var ErrNotDurable = errors.New("vault: applied but not confirmed durable")

// syncDirFn は、ディレクトリの fsync (テストで、失敗を注入する)。
var syncDirFn = syncDir

// writeDocument は、vault.json を、一時ファイル (0600・O_EXCL) への書き込み・fsync・rename・ディレクトリの fsync で、
// 原子的に置き換える。applied は、rename が成功した (ディスクが新しい内容になった) こと。applied なのに error のときは、
// ErrNotDurable を包む。
func writeDocument(dir string, d *document) (applied bool, err error) {
	data, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	if len(data) > maxFileSize {
		return false, fmt.Errorf("%w: 書く内容が大きすぎる", ErrInvalidInput)
	}
	tmp := filepath.Join(dir, tmpName)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, fileName)); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := syncDirFn(dir); err != nil {
		return true, fmt.Errorf("%w: %v", ErrNotDurable, err)
	}
	return true, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func validKind(k string) bool {
	return k == kindCredential || k == kindSigningKey || k == kindLoginState
}

// 項目の種類 (AAD に入る)。
const (
	kindCredential = "credential"
	kindSigningKey = "signing-key"
	kindLoginState = "login-state"
)

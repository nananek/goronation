//go:build unix

package credfile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nananek/goronation/core/credential"
	"github.com/nananek/goronation/hostfs"
)

const (
	// dirName は、状態ディレクトリの下の、資格情報のディレクトリの名前。
	dirName = "credentials"
	// defaultMaxValue は、値の長さの上限の既定 (バイト)。ファイルの上限 (maxFile) は、値に改行 1 つの余裕を足したもの。
	defaultMaxValue = 1024
	// largestMaxValue は、NewSized が許す、値の長さの上限の最大 (バイト)。
	largestMaxValue = 64 << 10
)

// fileLimit は、値の上限 maxValue に対する、ファイルの大きさの上限 (改行の余裕つき。既定では、従来の 4096 のまま)。
func fileLimit(maxValue int) int { return max(4096, maxValue+16) }

var (
	// ErrUnsafe は、ディレクトリ・ファイルが、安全な形でない (権限が緩い・所有者が違う・symlink・通常のファイルでない・ハードリンク) ときの error。
	// 読まない・書かない。errors.Is で判定する。
	ErrUnsafe = errors.New("credfile: unsafe")
	// ErrInvalid は、ファイルの中身・書こうとする値が、値の形 (印字できる ASCII 1 語・1024 バイト以下) でないときの error。
	ErrInvalid = errors.New("credfile: invalid value")
)

// afterOpen は、テストが、Token の、ファイルを開いた後・検査する前に、ファイルを置き換える場所。本番は nil。
var afterOpen func()

// beforeRename は、テストが、Save の、一時ファイルを書き終えた後・置き換える前に、ディレクトリを差し替える場所。本番は nil。
var beforeRename func()

// Store は、<state>/credentials の下に資格情報を置く credential.Source。
type Store struct {
	dir      string
	maxValue int // 値の長さの上限
	maxFile  int // 読むファイルの大きさの上限
}

var _ credential.Source = (*Store)(nil)

// New は、状態ディレクトリ stateDir (絶対・クリーン。制御文字なし) の Store を返す。何も作らない (作るのは Save)。
func New(stateDir string) (*Store, error) { return NewSized(stateDir, defaultMaxValue) }

// NewSized は、New で、値の長さの上限を maxValue (1〜64 KiB) にした Store を返す。既定の 1 KiB に収まらない値 (複数の passkey の
// 状態など) を、置く場所のため。上限を上げても、値の形の検査 (印字できる ASCII 1 語) と、ファイルの安全の検査は、そのまま。
func NewSized(stateDir string, maxValue int) (*Store, error) {
	if maxValue < 1 || maxValue > largestMaxValue {
		return nil, fmt.Errorf("credfile: 値の上限 %d が範囲外 (1〜%d)", maxValue, largestMaxValue)
	}
	switch {
	case !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir:
		return nil, fmt.Errorf("credfile: 状態ディレクトリ %q が、絶対・クリーンな path でない", stateDir)
	case strings.ContainsFunc(stateDir, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return nil, fmt.Errorf("credfile: 状態ディレクトリ %q が、制御文字を含む", stateDir)
	}
	return &Store{dir: filepath.Join(stateDir, dirName), maxValue: maxValue, maxFile: fileLimit(maxValue)}, nil
}

// Dir は、資格情報のディレクトリ (<state>/credentials)。
func (s *Store) Dir() string { return s.dir }

// Path は、name の資格情報のファイルの path (表示用。name の形は、確かめない)。
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// maxAttempts は、Token が、読む間に置き換わった (書き手の rename と重なった) ファイルを、読み直す回数の上限。
const maxAttempts = 20

// errReplaced は、読む間に、ファイルが置き換わったこと (Token が読み直す)。
var errReplaced = errors.New("credfile: replaced while reading")

// Token は、name の資格情報を返す。ディレクトリ・ファイルが無ければ、credential.ErrNotFound を包んだ error。安全な形でなければ ErrUnsafe、
// 中身が値の形でなければ ErrInvalid を包んだ error (どちらも、値を含まない)。
// 書き手 (Save) の rename と重なって、読む間にファイルが置き換わったときは、読み直す (maxAttempts 回。それでも変わり続ければ ErrUnsafe)。
func (s *Store) Token(ctx context.Context, name string) (credential.Secret, error) {
	if err := credential.CheckName(name); err != nil {
		return credential.Secret{}, err
	}
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return credential.Secret{}, err
		}
		v, err := s.readOnce(name)
		if !errors.Is(err, errReplaced) {
			return v, err
		}
		select { // 書き手の置き換えが済むのを、少しずつ長く待つ
		case <-time.After(time.Duration(i+1) * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return credential.Secret{}, fmt.Errorf("%w: %s が、読む間に書き換わり続けている", ErrUnsafe, s.Path(name))
}

// readOnce は、name を、1 回読む。読む間に置き換わったら errReplaced。
func (s *Store) readOnce(name string) (credential.Secret, error) {
	path := s.Path(name)
	root, err := s.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return credential.Secret{}, fmt.Errorf("%w: %s", credential.ErrNotFound, path)
		}
		return credential.Secret{}, err
	}
	defer root.Close()
	f, err := hostfs.Open(root, name, int64(s.maxFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return credential.Secret{}, fmt.Errorf("%w: %s", credential.ErrNotFound, path)
	case errors.Is(err, hostfs.ErrChanged): // 開いた fd と、名前の指すファイルが違う: 置き換わった (悪意なら、読み直しても変わり続ける)
		return credential.Secret{}, errReplaced
	case errors.Is(err, hostfs.ErrNotRegular):
		return credential.Secret{}, fmt.Errorf("%w: %s が、通常のファイルでない (symlink・FIFO・ディレクトリなど)", ErrUnsafe, path)
	case errors.Is(err, hostfs.ErrTooLarge):
		return credential.Secret{}, fmt.Errorf("%w: %s が大きすぎる (上限 %d バイト)", ErrInvalid, path, s.maxFile)
	case err != nil:
		return credential.Secret{}, fmt.Errorf("credfile: %s を開けない: %w", path, err)
	}
	defer f.Close()
	if afterOpen != nil {
		afterOpen()
	}
	fi, err := f.Stat() // 開いた fd の情報 (名前でなく、fd を検査する)
	if err != nil {
		return credential.Secret{}, fmt.Errorf("credfile: %s を調べられない: %w", path, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink == 0 { // 開いた後に、消された (置き換えられた)
		return credential.Secret{}, errReplaced
	}
	if err := checkStat(path, fi, false); err != nil {
		return credential.Secret{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(s.maxFile)+1))
	if err != nil {
		return credential.Secret{}, fmt.Errorf("credfile: %s を読めない: %w", path, err)
	}
	if len(data) > s.maxFile {
		return credential.Secret{}, fmt.Errorf("%w: %s が大きすぎる (上限 %d バイト)", ErrInvalid, path, s.maxFile)
	}
	v := strings.TrimSuffix(string(data), "\n") // 末尾の改行は、1 つだけ許す (Save が付ける)
	clear(data)
	if err := checkValue(v, s.maxValue); err != nil {
		return credential.Secret{}, fmt.Errorf("%w: %s の中身が、値の形でない", ErrInvalid, path)
	}
	return credential.New(v), nil
}

// Save は、name の資格情報を、原子的に書く (無ければ作る。あれば置き換える)。ディレクトリが無ければ 0700 で作る。
// 値が形でなければ ErrInvalid、ディレクトリが安全な形でなければ ErrUnsafe を包んだ error を返し、何も書かない。
func (s *Store) Save(name string, v credential.Secret) error {
	if err := credential.CheckName(name); err != nil {
		return err
	}
	value := v.Reveal()
	if err := checkValue(value, s.maxValue); err != nil {
		return fmt.Errorf("%w: 書こうとした値が、値の形でない", ErrInvalid)
	}
	if _, err := os.Lstat(s.dir); errors.Is(err, fs.ErrNotExist) {
		if err := mkdirPrivate(s.dir); err != nil {
			return fmt.Errorf("credfile: %s を作れない: %w", s.dir, err)
		}
	}
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()

	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return fmt.Errorf("credfile: 乱数を得られない: %w", err)
	}
	tmp := "." + name + ".tmp-" + hex.EncodeToString(r[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // O_EXCL: 既にあるもの (symlink を含む) には、書かない
	if err != nil {
		return fmt.Errorf("credfile: 一時ファイルを作れない: %w", err)
	}
	fail := func(what string, err error) error {
		f.Close()
		root.Remove(tmp)
		return fmt.Errorf("credfile: %s: %w", what, err)
	}
	buf := []byte(value + "\n")
	_, werr := f.Write(buf)
	clear(buf)
	if werr != nil {
		return fail("書けない", werr)
	}
	if err := f.Chmod(0o600); err != nil { // umask に依らず、0600 (fd への操作: 名前の差し替えに影響されない)
		return fail("権限を設定できない", err)
	}
	if err := f.Sync(); err != nil {
		return fail("fsync できない", err)
	}
	if err := f.Close(); err != nil {
		root.Remove(tmp)
		return fmt.Errorf("credfile: 閉じられない: %w", err)
	}
	if beforeRename != nil {
		beforeRename()
	}
	// 置き換えの直前に、ディレクトリが、開いたものと同じか確かめる (差し替えられていれば、別の場所へ置かない)。
	li, lerr := os.Lstat(s.dir)
	ri, rerr := root.Stat(".")
	if lerr != nil || rerr != nil || !os.SameFile(li, ri) {
		root.Remove(tmp)
		return fmt.Errorf("%w: %s が、書いている間に差し替えられた", ErrUnsafe, s.dir)
	}
	if err := os.Rename(filepath.Join(s.dir, tmp), filepath.Join(s.dir, name)); err != nil {
		root.Remove(tmp)
		return fmt.Errorf("credfile: %s に置き換えられない: %w", s.Path(name), err)
	}
	if d, err := root.Open("."); err == nil { // ディレクトリの記録も、ディスクへ (失敗しても、保存自体は済んでいる)
		d.Sync()
		d.Close()
	}
	return nil
}

// mkdirPrivate は、dir と、無い親を、0700 で作る (umask に依らない: umask が owner の bit を消しても、0700 にする)。作ったものだけを fchmod し、
// 既にあるものには触れない。fchmod は、O_NOFOLLOW で開いた fd に行う (作った直後に symlink に差し替えられても、辿らない)。
func mkdirPrivate(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := mkdirPrivate(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) { // 別のプロセスが、先に作った
			return nil
		}
		return err
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return syscall.Fchmod(fd, 0o700)
}

// openRoot は、資格情報のディレクトリを、fd で開いて検査する: symlink でなく、実行した利用者の所有で、0700 (group・other の bit なし)。
// 無ければ、fs.ErrNotExist を包んだ error。
func (s *Store) openRoot() (*os.Root, error) {
	li, err := os.Lstat(s.dir)
	if err != nil {
		return nil, err
	}
	if !li.IsDir() { // symlink も、ここで断る (Lstat は辿らない)
		return nil, fmt.Errorf("%w: %s が、ディレクトリでない (symlink など)", ErrUnsafe, s.dir)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("credfile: %s を開けない: %w", s.dir, err)
	}
	ri, err := root.Stat(".") // 開いた fd の情報
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("credfile: %s を調べられない: %w", s.dir, err)
	}
	if !os.SameFile(li, ri) {
		root.Close()
		return nil, fmt.Errorf("%w: %s が、開く間に差し替えられた", ErrUnsafe, s.dir)
	}
	if err := checkStat(s.dir, ri, true); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// checkStat は、fi (開いた fd の情報) が、実行した利用者の所有で、group・other・setuid・setgid・sticky の bit が無いか確かめる。
// ファイルは、ハードリンクが無い (Nlink が 1) ことも。
func checkStat(path string, fi fs.FileInfo, dir bool) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: %s の所有者を調べられない", ErrUnsafe, path)
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: %s の所有者が違う (uid %d)。chown で直す", ErrUnsafe, path, st.Uid)
	}
	if m := fi.Mode(); m.Perm()&0o077 != 0 || m&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		want := "600"
		if dir {
			want = "700"
		}
		return fmt.Errorf("%w: %s の権限が緩い (%04o)。chmod %s %s で直す", ErrUnsafe, path, m.Perm(), want, path)
	}
	if !dir && st.Nlink != 1 {
		return fmt.Errorf("%w: %s に、ハードリンクがある (%d)", ErrUnsafe, path, st.Nlink)
	}
	return nil
}

// checkValue は、v が、値の形 (印字できる ASCII 1 語: 0x21〜0x7e だけ・1〜maxValue バイト) か確かめる。error に、v を含めない。
func checkValue(v string, maxValue int) error {
	if v == "" || len(v) > maxValue {
		return ErrInvalid
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x21 || c > 0x7e {
			return ErrInvalid
		}
	}
	return nil
}

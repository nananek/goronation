//go:build linux

package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nananek/goronation/sandbox/bwrap"
)

// 既定の上限と時間。
const (
	// DefaultMaxBundleBytes は、取り出す bundle の大きさの上限。
	DefaultMaxBundleBytes = 512 << 20
	// DefaultGitTimeout は、檻の中の git (clone・bundle) 1 回の時間の上限。
	DefaultGitTimeout = 10 * time.Minute
)

// idRE は、セッション ID の形。<日付>-<時刻>-<乱数 6 桁>。外から来る ID は、必ずこの形を確かめる (path の要素に使うため)。
var idRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

// Store は、セッションを置く場所 (<state>/sessions) と、檻を起動するときの検証に使うホストの状態。
type Store struct {
	root string     // <state>/sessions (絶対)
	host bwrap.Host // bwrap.Spec の検証に使う

	maxBundle int64
	timeout   time.Duration
	newID     func() (string, error) // セッション ID の生成 (テストが差し替える)

	// afterBundle は、テストが、bundle を作った後・検査の前に、export/ の中を差し替える場所。本番は nil。
	afterBundle func(exportDir string)
}

// Session は、セッション 1 つのディレクトリ (すべて絶対 path)。
type Session struct {
	ID     string
	Dir    string // <state>/sessions/<id>
	Clone  string // private clone (檻が rw で使う作業ディレクトリ)
	Run    string // 実行時のファイル (egress のソケットなど)
	Export string // 成果の bundle を置く
}

// DefaultStateDir は、状態を置く既定のディレクトリ ($XDG_STATE_HOME/goro。無ければ ~/.local/state/goro)。
func DefaultStateDir() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "goro"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("session: HOME を決められない: %w", err)
	}
	return filepath.Join(home, ".local", "state", "goro"), nil
}

// NewStore は、stateDir (絶対・クリーン) の下に、<stateDir>/sessions を 0700 で作り、Store を返す。
// host は、bwrap.Spec の検証に使う (bwrap.CurrentHost)。
func NewStore(stateDir string, host bwrap.Host) (*Store, error) {
	if err := checkAbs("stateDir", stateDir); err != nil {
		return nil, err
	}
	root := filepath.Join(stateDir, "sessions")
	for _, d := range []string{stateDir, root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("session: %w", err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("session: %w", err)
		}
	}
	return &Store{root: root, host: host, maxBundle: DefaultMaxBundleBytes, timeout: DefaultGitTimeout, newID: newID}, nil
}

// Get は、ID のセッションを返す。ID の形が正しく、本物のディレクトリ (symlink でない) でなければ error。
func (s *Store) Get(id string) (*Session, error) {
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("session: ID %q の形が正しくない", id)
	}
	fi, err := os.Lstat(filepath.Join(s.root, id))
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("session: %s がディレクトリではない", id)
	}
	return s.layout(id), nil
}

// layout は、ID のセッションのディレクトリ構成 (作らない)。
func (s *Store) layout(id string) *Session {
	dir := filepath.Join(s.root, id)
	return &Session{
		ID: id, Dir: dir,
		Clone: filepath.Join(dir, "clone"), Run: filepath.Join(dir, "run"), Export: filepath.Join(dir, "export"),
	}
}

// newID は、新しいセッション ID。
func newID() (string, error) {
	var r [3]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", fmt.Errorf("session: 乱数を得られない: %w", err)
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(r[:]), nil
}

// checkAbs は、p が絶対・クリーンで、制御文字を含まないことを確かめる (檻へ bind する path と、コマンドに書く path の前提)。
func checkAbs(what, p string) error {
	switch {
	case !filepath.IsAbs(p):
		return fmt.Errorf("session: %s %q が絶対 path ではない", what, p)
	case filepath.Clean(p) != p:
		return fmt.Errorf("session: %s %q がクリーンではない", what, p)
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("session: %s %q が制御文字を含む", what, p)
	}
	return nil
}

// under は、p が root と等しいか、root の下にあるか (どちらもクリーンな絶対 path)。
func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

//go:build linux

package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nananek/goronation/hostfs"
)

const (
	// homeKeyFile は、セッションのディレクトリの直下に置く、元の repo のキー (中身は "<16 桁の 16 進>\n")。エージェントの HOME を、
	// repo ごとに分けるための名前 (呼び手が <state>/agents/<エージェント>/homes/<キー>/ にする)。檻に bind するのは clone・run・export だけで、
	// この場所は檻から見えない (ホストだけが書く)。
	homeKeyFile = "homekey"
	// maxHomeKeyFile は、homeKeyFile を読む上限 (バイト)。
	maxHomeKeyFile = 64
)

// homeKeyRE は、repo のキーの形。外から来る値 (ファイルの中身) は、必ずこの形を確かめる (path の要素に使うため)。
var homeKeyRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// RepoKey は、元の repo の実 path (symlink を解決した、絶対・クリーンな path) から、repo のキーを作る: sha256 の先頭 8 バイトの 16 進 16 桁。
// 同じ repo (同じ実 path) は、いつも同じキー。別の path の repo は、別のキー (衝突は、64 bit の偶然だけ)。
func RepoKey(realPath string) string {
	sum := sha256.Sum256([]byte(realPath))
	return hex.EncodeToString(sum[:8])
}

// writeHomeKey は、repo のキーを、セッションのディレクトリに記録する。
func writeHomeKey(sess *Session, key string) error {
	if err := os.WriteFile(filepath.Join(sess.Dir, homeKeyFile), []byte(key+"\n"), 0o600); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

// HomeKey は、sess を作った repo のキー (Create が記録したもの) を返す。記録が無い (HOME を repo ごとに分ける前に作った) ときは、空で、
// error ではない: 呼び手が、そのセッションを、共有の HOME で動かさない判断をする。記録が読めない・形が正しくない (通常のファイルでない・
// 大きすぎる・複数行・16 桁の 16 進でないなど) ときは error: 壊れた記録から、別の repo の HOME を選ばない。
func (s *Store) HomeKey(sess *Session) (string, error) {
	root, err := os.OpenRoot(sess.Dir)
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	defer root.Close()
	b, err := hostfs.ReadFile(root, homeKeyFile, maxHomeKeyFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("session: repo のキーの記録 (%s) を読めない: %w", homeKeyFile, err)
	}
	key, rest, _ := strings.Cut(string(b), "\n")
	if rest != "" || !homeKeyRE.MatchString(key) {
		return "", fmt.Errorf("session: repo のキーの記録 (%s) の中身が正しくない: %q", homeKeyFile, cleanLabel(string(b)))
	}
	return key, nil
}

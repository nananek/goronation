//go:build linux

package session

import (
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
	// agentFile は、セッションのディレクトリの直下に置く、セッションを作ったエージェントの名前 (中身は "<名前>\n")。
	// 檻に bind するのは clone・run・export だけで、この場所は檻から見えない (ホストだけが書く)。
	agentFile = "agent"
	// maxAgentFile は、agentFile を読む上限 (バイト)。
	maxAgentFile = 64
	// UnknownAgent は、List が、記録を読めない・正しくないセッションの Agent に入れる値。
	UnknownAgent = "?"
)

// agentRE は、エージェントの名前の形。外から来る値 (ファイルの中身) は、必ずこの形を確かめる。
var agentRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// checkAgent は、name がエージェントの名前の形か確かめる。
func checkAgent(name string) error {
	if !agentRE.MatchString(name) {
		return fmt.Errorf("session: Agent %q が正しくない (小文字・数字・- の、32 文字以下の名前)", name)
	}
	return nil
}

// writeAgent は、セッションを作ったエージェントの名前を、セッションのディレクトリに記録する。
func writeAgent(sess *Session, name string) error {
	if err := os.WriteFile(filepath.Join(sess.Dir, agentFile), []byte(name+"\n"), 0o600); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

// Agent は、sess を作ったエージェントの名前 (Create の CreateOptions.Agent) を返す。記録が無い (Agent を記録する前に作った、
// または Agent を渡さずに作った) ときは、空で、error ではない。記録が読めない・形が正しいものでない (通常のファイルでない・
// 大きすぎる・複数行など) ときは error: 記録が壊れたセッションを、黙って別のエージェントのものとして扱わない。
func (s *Store) Agent(sess *Session) (string, error) {
	return readAgent(sess.Dir)
}

// readAgent は、セッションのディレクトリ dir の記録を、hostfs で (通常のファイルで、上限以下のものだけ) 読む。
func readAgent(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	defer root.Close()
	b, err := hostfs.ReadFile(root, agentFile, maxAgentFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("session: エージェントの記録 (%s) を読めない: %w", agentFile, err)
	}
	name, rest, _ := strings.Cut(string(b), "\n")
	if rest != "" || !agentRE.MatchString(name) {
		return "", fmt.Errorf("session: エージェントの記録 (%s) の中身が正しくない: %q", agentFile, cleanLabel(string(b)))
	}
	return name, nil
}

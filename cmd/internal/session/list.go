//go:build linux

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nananek/goronation/hostfs"
)

const (
	// repoFile は、セッションのディレクトリの直下に置く、元の repo の名前 (一覧に出すだけ)。檻には見せない場所にある。
	repoFile = "repo"
	// maxRepoLabel は、元の repo の名前を、書く・読む上限 (バイト)。
	maxRepoLabel = 200
)

// Info は、一覧に出す、セッション 1 つの情報。
type Info struct {
	ID string
	// Created は、セッションを作った時刻 (ID から読む。UTC)。
	Created time.Time
	// Repo は、元の repo のディレクトリ名。記録が無い・読めないときは空。
	Repo string
	// Agent は、セッションを作ったエージェントの名前。記録が無いときは空。読めない・正しくないときは UnknownAgent。
	Agent string
}

// List は、セッションを、古い順に返す。名前が ID の形でないもの、本物のディレクトリでないもの (symlink を含む) は、数えない。
func (s *Store) List() ([]Info, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	var out []Info
	for _, e := range ents {
		id := e.Name()
		if !idRE.MatchString(id) || e.Type() != os.ModeDir {
			continue
		}
		created, err := time.Parse("20060102-150405", id[:15])
		if err != nil {
			continue
		}
		dir := filepath.Join(s.root, id)
		agent, err := readAgent(dir)
		if err != nil {
			agent = UnknownAgent
		}
		out = append(out, Info{ID: id, Created: created, Repo: readRepoLabel(dir), Agent: agent})
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// writeRepoLabel は、元の repo の名前 (ディレクトリ名) を、セッションのディレクトリに記録する。
func writeRepoLabel(sess *Session, repo string) error {
	if err := os.WriteFile(filepath.Join(sess.Dir, repoFile), []byte(cleanLabel(filepath.Base(repo))+"\n"), 0o600); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

// readRepoLabel は、セッションのディレクトリ dir の記録を、hostfs で (通常のファイルで、上限以下のものだけ) 読む。読めなければ空。
func readRepoLabel(dir string) string {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ""
	}
	defer root.Close()
	b, err := hostfs.ReadFile(root, repoFile, maxRepoLabel*4)
	if err != nil {
		return ""
	}
	label, _, _ := strings.Cut(string(b), "\n")
	return cleanLabel(label)
}

// cleanLabel は、s を、端末に出しても安全な 1 行 (制御文字・書式制御・行区切り・不正な UTF-8 を ? にし、上限で切ったもの) にする。
func cleanLabel(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (0x80 <= r && r < 0xa0) || r == 0xfffd || r == 0x2028 || r == 0x2029 ||
			(0x200b <= r && r <= 0x200f) || (0x202a <= r && r <= 0x202e) || (0x2060 <= r && r <= 0x206f) || r == 0xfeff {
			return '?'
		}
		return r
	}, s)
	if len(s) > maxRepoLabel {
		s = strings.ToValidUTF8(s[:maxRepoLabel], "")
	}
	return s
}

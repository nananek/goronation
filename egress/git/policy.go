package git

import (
	"io"
	"strings"
)

// maxRefLen は、許す ref の全体の長さ (バイト)。
const maxRefLen = 200

// Policy は、1 セッションが git で触れてよい範囲: 1 つの repo と、その中の、セッション専用の名前空間 refs/heads/goronation/<Session>/ だけ。
type Policy struct {
	Repo    Repo
	Session string
}

// NewPolicy は、repo とセッション ID の Policy を作る。セッション ID は、ref の 1 要素として正しい (1〜64 文字の [A-Za-z0-9._-]) 必要がある。
func NewPolicy(repo Repo, session string) (Policy, error) {
	if !validOwner(repo.Owner) || !validRepoName(repo.Name) {
		return Policy{}, reject(CodeBadRoute, "repo %s の形が正しくない", q(repo.String()))
	}
	if len(session) > 64 || !validSegment(session) {
		return Policy{}, reject(CodeRefNotAllowed, "セッション ID %s が、ref の要素として正しくない", q(session))
	}
	return Policy{Repo: repo, Session: session}, nil
}

// Prefix は、許す ref の接頭辞 "refs/heads/goronation/<Session>/" を返す。
func (p Policy) Prefix() string { return "refs/heads/goronation/" + p.Session + "/" }

// CheckRepo は、r が許した repo かを確かめる (大文字小文字は区別しない)。
func (p Policy) CheckRepo(r Repo) error {
	if !p.Repo.Equal(r) {
		return reject(CodeRepoNotAllowed, "repo %s は許可していない", q(r.String()))
	}
	return nil
}

// CheckRef は、ref が、許した名前空間 (Prefix の下) の、正しい名前かを確かめる。
// Prefix の後は、1 つ以上の要素で、各要素は [A-Za-z0-9._-] だけ・空でない・. で始まらない・. で終わらない・.lock で終わらない・.. を含まない。全体は 200 バイトまで。
func (p Policy) CheckRef(ref string) error {
	rest, ok := strings.CutPrefix(ref, p.Prefix())
	if !ok {
		return reject(CodeRefNotAllowed, "ref %s は %s の下ではない", q(ref), p.Prefix())
	}
	if len(ref) > maxRefLen {
		return reject(CodeRefNotAllowed, "ref %s が %d バイトを超える", q(ref), maxRefLen)
	}
	for _, seg := range strings.Split(rest, "/") {
		if !validSegment(seg) {
			return reject(CodeRefNotAllowed, "ref %s の要素 %s が正しくない", q(ref), q(seg))
		}
	}
	return nil
}

// CheckBranch は、branch 名 name (refs/heads/ を除く) が、CheckRef を通るかを確かめる。PR の head に使う。
func (p Policy) CheckBranch(name string) error { return p.CheckRef("refs/heads/" + name) }

// Check は、push の全コマンドが、削除でなく、許した名前空間の ref かを確かめる。1 つでも外れれば、全体を断る (一部だけ通さない)。
func (p Policy) Check(push *Push) error {
	for _, c := range push.Commands {
		if c.New == zeroOID {
			return reject(CodeDelete, "ref %s の削除は受けない", q(c.Ref))
		}
		if err := p.CheckRef(c.Ref); err != nil {
			return err
		}
	}
	return nil
}

// ReadPush は、ParseCommands と Check を続けて行う。通ったときだけ Push を返す (断られた要求のバイト列は、返さない)。
func (p Policy) ReadPush(r io.Reader, lim Limits) (*Push, error) {
	push, err := ParseCommands(r, lim)
	if err != nil {
		return nil, err
	}
	if err := p.Check(push); err != nil {
		return nil, err
	}
	return push, nil
}

// CheckBranchName は、name が、正しい branch 名 (refs/heads/ の後) か、名前空間を問わずに確かめる。PR の base に使う。
// 各要素は Policy.CheckRef と同じ規則で、全体は 200 バイトまで、先頭は - でない。
func CheckBranchName(name string) error {
	if name == "" || len(name) > maxRefLen || name[0] == '-' {
		return reject(CodeRefNotAllowed, "branch 名 %s が正しくない", q(name))
	}
	for _, seg := range strings.Split(name, "/") {
		if !validSegment(seg) {
			return reject(CodeRefNotAllowed, "branch 名 %s の要素 %s が正しくない", q(name), q(seg))
		}
	}
	return nil
}

// validSegment は、ref の 1 要素として、許す形か。
func validSegment(s string) bool {
	if s == "" || s[0] == '.' || s[len(s)-1] == '.' || strings.HasSuffix(s, ".lock") || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(isAlnum(c) || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

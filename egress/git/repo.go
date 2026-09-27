package git

import "strings"

// Repo は、GitHub の repo (owner/name)。名前は、大文字小文字を区別せずに比べる (GitHub がそう扱うため)。
type Repo struct {
	Owner, Name string
}

// ParseRepo は、"owner/name" を Repo にする。owner と name の形が正しくなければ、CodeBadRoute の *Error を返す。
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !validOwner(owner) || !validRepoName(name) {
		return Repo{}, reject(CodeBadRoute, "repo %s が owner/name の形ではない", q(s))
	}
	return Repo{Owner: owner, Name: name}, nil
}

// String は、"owner/name" を返す。
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Equal は、r と o が同じ repo か (owner・name とも、ASCII の大文字小文字を区別しない)。
func (r Repo) Equal(o Repo) bool {
	return strings.EqualFold(r.Owner, o.Owner) && strings.EqualFold(r.Name, o.Name)
}

// validOwner は、GitHub の owner (user・org) の名前か: [A-Za-z0-9-] の 1〜39 文字で、- で始まり終わらない。
func validOwner(s string) bool {
	if len(s) == 0 || len(s) > 39 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(isAlnum(c) || c == '-') {
			return false
		}
	}
	return true
}

// validRepoName は、GitHub の repo の名前か: [A-Za-z0-9._-] の 1〜100 文字で、"." と ".." ではなく、".git" で終わらない
// (URL の ".git" と紛れるため。GitHub も、この名前の repo は作れない)。
func validRepoName(s string) bool {
	if len(s) == 0 || len(s) > 100 || s == "." || s == ".." || strings.HasSuffix(strings.ToLower(s), ".git") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(isAlnum(c) || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

package git

import "strings"

// PathPrefix は、檻の git が、https://github.com/ の代わりに使う URL の path の接頭辞。
// 檻の設定は、loopback の egress の URL にこの接頭辞を付けたものを、url.<その URL>.insteadOf に置き換え元 (https://github.com/) を書く。
const PathPrefix = "/git/github.com/"

// Op は、許す要求の種類 (4 つだけ)。
type Op int

// 許す要求の種類。
const (
	OpAdvertiseUpload  Op = iota + 1 // GET info/refs?service=git-upload-pack (fetch・clone の最初)
	OpAdvertiseReceive               // GET info/refs?service=git-receive-pack (push の最初)
	OpUploadPack                     // POST git-upload-pack (fetch・clone の本体)
	OpReceivePack                    // POST git-receive-pack (push の本体。本文を ParseCommands で検査する)
)

// String は、監査に出す名前を返す。
func (o Op) String() string {
	switch o {
	case OpAdvertiseUpload:
		return "advertise-upload-pack"
	case OpAdvertiseReceive:
		return "advertise-receive-pack"
	case OpUploadPack:
		return "upload-pack"
	case OpReceivePack:
		return "receive-pack"
	}
	return "unknown"
}

// Writes は、o が、上流の repo を書き換えうる要求か (push の 2 つ)。
func (o Op) Writes() bool { return o == OpAdvertiseReceive || o == OpReceivePack }

// RequestType は、POST の本文の Content-Type (GET は空)。
func (o Op) RequestType() string {
	switch o {
	case OpUploadPack:
		return "application/x-git-upload-pack-request"
	case OpReceivePack:
		return "application/x-git-receive-pack-request"
	}
	return ""
}

// Route は、検査を通した git の要求。
type Route struct {
	Repo Repo
	Op   Op
}

// Upstream は、上流 (github.com) に送る path と query を返す。要求の path は使わず、検査した値から作る (owner・name は小文字にする)。
func (r Route) Upstream() string {
	base := "/" + strings.ToLower(r.Repo.Owner) + "/" + strings.ToLower(r.Repo.Name) + ".git/"
	switch r.Op {
	case OpAdvertiseUpload:
		return base + "info/refs?service=git-upload-pack"
	case OpAdvertiseReceive:
		return base + "info/refs?service=git-receive-pack"
	case OpUploadPack:
		return base + "git-upload-pack"
	case OpReceivePack:
		return base + "git-receive-pack"
	}
	return ""
}

// ParseRoute は、要求の method と request-target (origin-form) を検査し、Route にする。
// PathPrefix で始まり、<owner>/<name>.git/ の後が、次の 4 つのどれかに完全一致するものだけを通す。
// GET info/refs?service=git-upload-pack・GET info/refs?service=git-receive-pack・POST git-upload-pack・POST git-receive-pack。
// %・#・..・//・制御文字・非 ASCII・余計な query は、断る (CodeBadRoute)。
func ParseRoute(method, target string) (Route, error) {
	path, query, hasQuery := strings.Cut(target, "?")
	if !strings.HasPrefix(path, PathPrefix) {
		return Route{}, reject(CodeBadRoute, "path %s は %s で始まらない", q(path), PathPrefix)
	}
	// 文字の検査は、別に置かない: owner と name は限られた文字だけで、残りは、固定の文字列との完全一致で比べる。
	parts := strings.Split(path[len(PathPrefix):], "/")
	// <owner>/<name>.git/ の後は、git-upload-pack・git-receive-pack・info/refs のどれか
	if len(parts) < 2 {
		return Route{}, reject(CodeBadRoute, "path %s の形が正しくない", q(path))
	}
	name, ok := strings.CutSuffix(parts[1], ".git")
	if !ok || !validOwner(parts[0]) || !validRepoName(name) {
		return Route{}, reject(CodeBadRoute, "repo %s の形が正しくない", q(parts[0]+"/"+parts[1]))
	}
	repo := Repo{Owner: parts[0], Name: name}
	tail := strings.Join(parts[2:], "/")
	switch {
	case method == "GET" && tail == "info/refs":
		switch query {
		case "service=git-upload-pack":
			return Route{Repo: repo, Op: OpAdvertiseUpload}, nil
		case "service=git-receive-pack":
			return Route{Repo: repo, Op: OpAdvertiseReceive}, nil
		}
		return Route{}, reject(CodeBadRoute, "query %s は service=git-upload-pack か service=git-receive-pack だけ", q(query))
	case method == "POST" && !hasQuery && tail == "git-upload-pack":
		return Route{Repo: repo, Op: OpUploadPack}, nil
	case method == "POST" && !hasQuery && tail == "git-receive-pack":
		return Route{Repo: repo, Op: OpReceivePack}, nil
	}
	return Route{}, reject(CodeBadRoute, "%s %s は許す要求ではない", q(method), q(tail))
}

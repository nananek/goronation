package github

import (
	"strings"

	"github.com/nananek/goronation/egress/git"
)

// PathPrefix は、檻の goro pr create が、PR を作るために送る先の path の接頭辞。
const PathPrefix = "/github-api/"

// ParsePullRoute は、要求の method と request-target (origin-form) を検査し、PR を作る repo を返す。
// 通すのは、POST /github-api/repos/<owner>/<name>/pulls だけ (query なし)。他の method・path (PR の更新・undraft・issue・別の API) は、全て断る。
// %・#・..・//・制御文字・非 ASCII は断る (CodeBadRoute)。
func ParsePullRoute(method, target string) (git.Repo, error) {
	// 文字の検査は、別に置かない: owner と name は限られた文字だけで、残りは、固定の文字列との完全一致で比べる。
	rest, ok := strings.CutPrefix(target, PathPrefix+"repos/")
	if !ok || method != "POST" {
		return git.Repo{}, reject(CodeBadRoute, "%s %s は許す要求ではない", q(method), q(target))
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[2] != "pulls" {
		return git.Repo{}, reject(CodeBadRoute, "path %s の形が正しくない", q(target))
	}
	repo, err := git.ParseRepo(parts[0] + "/" + parts[1])
	if err != nil {
		return git.Repo{}, reject(CodeBadRoute, "repo %s の形が正しくない", q(parts[0]+"/"+parts[1]))
	}
	return repo, nil
}

// PullsPath は、上流 (api.github.com) の PR の作成先の path を返す。要求の path は使わず、検査した repo から作る (小文字)。
func PullsPath(r git.Repo) string { return RepoPath(r) + "/pulls" }

// RepoPath は、上流の repo の情報 (既定の branch を引く) の path を返す。
func RepoPath(r git.Repo) string {
	return "/repos/" + strings.ToLower(r.Owner) + "/" + strings.ToLower(r.Name)
}

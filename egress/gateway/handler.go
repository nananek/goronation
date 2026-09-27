package gateway

import (
	"net/http"
	"strings"

	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

// ServeHTTP は、経路の接頭辞で、git smart-HTTP (git.PathPrefix) と、PR 作成 (github.PathPrefix) に振り分ける。
// どちらでもなければ 404 (中継しない)。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.RequestURI, git.PathPrefix):
		h.serveGit(w, r)
	case strings.HasPrefix(r.RequestURI, github.PathPrefix):
		h.servePull(w, r)
	default:
		http.NotFound(w, r)
	}
}

// gitStatus は、git.Error の Code (Reason) を、応答の状態コードにする。空 (git.Error でない error) は 400。
func gitStatus(code string) int {
	switch code {
	case git.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case git.CodeDelete, git.CodeRefNotAllowed, git.CodeRepoNotAllowed:
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

// pullStatus は、github.Error の Code (Reason) を、応答の状態コードにする。
func pullStatus(code string) int {
	switch code {
	case github.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case github.CodeRepoNotAllowed, github.CodeHeadNotAllowed:
		return http.StatusForbidden
	case github.CodeQuota:
		return http.StatusTooManyRequests
	case github.CodeBadResponse:
		return http.StatusBadGateway
	default:
		return http.StatusBadRequest
	}
}

// orInternal は、code が空 (理由が分からない拒否) なら "internal" にする (監査に、空の reason を残さない)。
func orInternal(code string) string {
	if code == "" {
		return "internal"
	}
	return code
}

// rejectGit は、git 側の拒否を、監査に残して応答する。応答の本文には、code 以上の詳細 (error の文言) を出さない
// (檻に、内部の path・実装の詳細を渡さない)。
func (h *Handler) rejectGit(w http.ResponseWriter, repo git.Repo, op string, status int, reason string) {
	h.logAudit(auditRecord{Event: "git", Repo: repoString(repo), Op: op, Status: status, Reason: reason})
	http.Error(w, http.StatusText(status), status)
}

// rejectPull は、PR 側の拒否を、監査に残して応答する。
func (h *Handler) rejectPull(w http.ResponseWriter, repo git.Repo, status int, reason string) {
	h.logAudit(auditRecord{Event: "pull", Repo: repoString(repo), Status: status, Reason: reason})
	http.Error(w, http.StatusText(status), status)
}

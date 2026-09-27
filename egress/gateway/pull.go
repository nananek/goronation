package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

// servePull は、github.PathPrefix (PR 作成) の要求を処理する。repo の検査は、ParsePull の中で行われる (target)。
func (h *Handler) servePull(w http.ResponseWriter, r *http.Request) {
	repo, err := github.ParsePullRoute(r.Method, r.RequestURI)
	if err != nil {
		h.rejectPull(w, git.Repo{}, pullStatus(github.Reason(err)), orInternal(github.Reason(err)))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, github.MaxRequestBytes))
	if err != nil {
		h.rejectPull(w, repo, pullStatus(github.CodeTooLarge), "too-large")
		return
	}
	pull, err := h.pull.ParsePull(repo, body)
	if err != nil {
		h.rejectPull(w, repo, pullStatus(github.Reason(err)), orInternal(github.Reason(err)))
		return
	}
	if pull.Base == "" {
		branch, err := h.defaultBranch(r.Context(), pull.Repo)
		if err != nil {
			h.rejectPull(w, repo, http.StatusBadGateway, "default-branch")
			return
		}
		if pull, err = pull.WithBase(branch); err != nil {
			h.rejectPull(w, repo, http.StatusBadGateway, "default-branch-invalid")
			return
		}
	}
	if err := h.quota.Take(); err != nil {
		h.rejectPull(w, repo, pullStatus(github.Reason(err)), orInternal(github.Reason(err)))
		return
	}
	out, err := pull.JSON()
	if err != nil {
		h.rejectPull(w, repo, http.StatusInternalServerError, "internal")
		return
	}
	h.relayPull(w, r, pull.Repo, out)
}

// defaultBranch は、上流の repo 情報を取り、既定の branch 名を返す。
func (h *Handler) defaultBranch(ctx context.Context, repo git.Repo) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.apiBase+github.RepoPath(repo), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", upstreamUserAgent)
	tok, err := h.creds.Token(ctx, CredentialName)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway: repo 情報: 上流が %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	return github.DefaultBranch(b)
}

// relayPull は、検査した本文 body (Pull.JSON の結果) を、上流の PR 作成に送り、応答を検査して (github.ParseResult)、
// number・html_url だけを、檻に返す。
func (h *Handler) relayPull(w http.ResponseWriter, r *http.Request, repo git.Repo, body []byte) {
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.apiBase+github.PullsPath(repo), bytes.NewReader(body))
	if err != nil {
		h.rejectPull(w, repo, http.StatusInternalServerError, "internal")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/vnd.github+json")
	upReq.Header.Set("User-Agent", upstreamUserAgent)
	tok, err := h.creds.Token(r.Context(), CredentialName)
	if err != nil {
		h.rejectPull(w, repo, http.StatusBadGateway, "credential")
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+tok.Reveal())

	start := time.Now()
	resp, err := h.client.Do(upReq)
	if err != nil {
		h.rejectPull(w, repo, http.StatusBadGateway, "upstream")
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		h.rejectPull(w, repo, http.StatusBadGateway, "upstream-read")
		return
	}
	ms := time.Since(start).Milliseconds()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.logAudit(auditRecord{Event: "pull", Repo: repo.String(), Status: resp.StatusCode, Reason: "upstream-error", Ms: ms})
		http.Error(w, "pull request を作れなかった", http.StatusBadGateway)
		return
	}
	result, err := github.ParseResult(respBody, repo)
	if err != nil {
		h.logAudit(auditRecord{Event: "pull", Repo: repo.String(), Status: resp.StatusCode, Reason: "bad-response", Ms: ms})
		http.Error(w, "", http.StatusBadGateway)
		return
	}
	reason := ""
	if !result.Draft {
		reason = "not-draft" // 上流が draft を外して作った (強制が効かなかった)。監査に残す
	}
	h.logAudit(auditRecord{Event: "pull", Repo: repo.String(), Status: http.StatusCreated, Reason: reason, Ms: ms})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(result.JSON())
}

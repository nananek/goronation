//go:build linux

package main

import (
	"errors"
	"io"
	"net/http"
)

// sessionInfoDTO は、GET /api/sessions が返す JSON の形 (session.Info を、小文字のフィールド名に
// 変換するだけ。cmd/internal/session は無改造のまま使う)。
type sessionInfoDTO struct {
	ID      string `json:"id"`
	Created string `json:"created"`
	Repo    string `json:"repo"`
	Agent   string `json:"agent"`
}

// handleSessionsPage は、requireSession で保護された、セッション一覧・repo のファイルブラウザの
// ページ。
func (s *webServer) handleSessionsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, sessionsHTML)
}

// handleSessionsList は、GET /api/sessions: 既存のセッションを、古い順の JSON 配列で返す。
func (s *webServer) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	list, err := s.sessions.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "セッションの一覧を取得できない")
		return
	}
	out := make([]sessionInfoDTO, len(list))
	for i, in := range list {
		agent := in.Agent
		if agent == "" {
			agent = legacySessionAgent
		}
		out[i] = sessionInfoDTO{ID: in.ID, Created: in.Created.Format("2006-01-02T15:04:05Z"), Repo: in.Repo, Agent: agent}
	}
	writeJSON(w, http.StatusOK, out)
}

type repoStartRequest struct {
	Repo string `json:"repo"`
}
type repoStartResponse struct {
	ID string `json:"id"`
}

// handleRepoStart は、POST /api/repos/start: --repos-dir 直下の repo (name) を、goro serve --repo で
// 新しいセッションとして起動し、新しいセッションの ID を返す (ブラウザは、それで /s/<id> へ移る)。
func (s *webServer) handleRepoStart(w http.ResponseWriter, r *http.Request) {
	var req repoStartRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "壊れた要求")
		return
	}
	repoPath, err := resolveRepoName(s.reposDir, req.Repo)
	if err != nil {
		var he httpError
		if errors.As(err, &he) {
			writeError(w, he.status, he.label)
			return
		}
		writeError(w, http.StatusBadRequest, "repo を解決できない")
		return
	}
	id, err := spawnServe(s.stateDir, s.self, []string{"--repo", repoPath})
	if err != nil {
		writeError(w, http.StatusBadGateway, "セッションを起動できない")
		return
	}
	writeJSON(w, http.StatusOK, repoStartResponse{ID: id})
}

// handleTerminalPage は、requireSession で保護された、端末ビューのページ (/s/{id})。中の terminal.js
// が、自分の path から WebSocket の接続先 (/s/{id}/ws) を組み立てる。id 自体の存在確認はしない
// (存在しない・終わったセッションなら、WebSocket 側が閉じて、terminal.js が案内を出す)。
func (s *webServer) handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, terminalHTML)
}

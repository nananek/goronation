//go:build linux

package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
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

// handleRepoStart は、POST /api/repos/start: --repos-dir 直下の repo (name) を、goronation serve --repo で
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
//
// xterm.js が実行時に動的生成する <style> (ADR 0007 のローカルパッチで対応済み) のために、この
// ルートの応答に限り、リクエストごとに新しい CSP nonce を発行する。securityHeaders が先に設定した
// 既定の Content-Security-Policy を、Write 前にここで上書きする (net/http は WriteHeader/Write 前
// ならヘッダーを何度でも書き換えられる)。他のルートは既定のまま変えない。
//
// baseContentSecurityPolicy への文字列連結ではなく cspWithoutStyleSrc から組み立てる: style-src
// directive を自分で明示するので、baseContentSecurityPolicy の並びが将来変わっても、nonce が
// 意図しない directive に付く事故が起きない。
func (s *webServer) handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	nonce, err := newCSPNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "nonce を生成できない")
		return
	}
	w.Header().Set("Content-Security-Policy", cspWithoutStyleSrc+"; style-src 'self' 'nonce-"+nonce+"'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	terminalHTMLTmpl.Execute(w, terminalPageData{Nonce: nonce})
}

// newCSPNonce は、CSP nonce (RFC の要求どおり、リクエストごとに新しく生成する、予測不能な値) を作る。
// 16 バイトの crypto/rand を base64 にエンコードする。標準の base64.StdEncoding ではなく
// URLEncoding (「+」「/」の代わりに「-」「_」を使う) を選んでいるのは、CSP nonce-source の
// ABNF ([A-Za-z0-9+/\-_]+={0,2}) がどちらの変種も許すことに加え、「+」だと html/template が
// <meta name="csp-nonce" content="..."> の属性値として書き出すときに `&#43;` へエスケープして
// しまい (html/template が「+」を UTF-7 対策として無条件にエスケープする既知の挙動)、CSP ヘッダーの
// 生の値と <meta> の HTML ソース上の文字列表現が食い違うため (ブラウザの DOM 越しに読めば
// エスケープは解決済みの値になるので実害は無いが、CI のテストが HTML ソースをそのまま文字列比較して
// いて偽陽性で落ちた。URLEncoding ならその食い違いの余地自体が無くなる)。
func newCSPNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand から nonce を読めない: %w", err)
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}

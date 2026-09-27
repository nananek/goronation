package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/nananek/goronation/egress/git"
)

// serveGit は、git.PathPrefix の要求を処理する。経路と repo の検査 (AuthorizedRoute) を、必ず最初に通す。
func (h *Handler) serveGit(w http.ResponseWriter, r *http.Request) {
	route, err := h.push.AuthorizedRoute(r.Method, r.RequestURI)
	if err != nil {
		h.rejectGit(w, git.Repo{}, "", gitStatus(git.Reason(err)), orInternal(git.Reason(err)))
		return
	}
	op := route.Op.String()
	switch route.Op {
	case git.OpAdvertiseUpload, git.OpAdvertiseReceive:
		h.relayGit(w, r, route, nil)
	case git.OpUploadPack:
		h.relayGit(w, r, route, http.MaxBytesReader(w, r.Body, h.maxPack))
	case git.OpReceivePack:
		h.serveReceivePack(w, r, route)
	default:
		h.rejectGit(w, route.Repo, op, http.StatusNotFound, "unknown-op")
	}
}

// serveReceivePack は、POST git-receive-pack を処理する: コマンド部を検査し (git.Policy.ReadPush)、通ったものだけを、
// 検査したバイト列 (Raw) + 続きの本文 (pack) のまま、上流に中継する。
func (h *Handler) serveReceivePack(w http.ResponseWriter, r *http.Request, route git.Route) {
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		h.rejectGit(w, route.Repo, "receive-pack", http.StatusUnsupportedMediaType, "content-encoding")
		return
	}
	limited := http.MaxBytesReader(w, r.Body, h.maxPack)
	push, err := h.push.ReadPush(limited, git.Limits{})
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.rejectGit(w, route.Repo, "receive-pack", http.StatusRequestEntityTooLarge, "pack-too-large")
			return
		}
		h.rejectGit(w, route.Repo, "receive-pack", gitStatus(git.Reason(err)), orInternal(git.Reason(err)))
		return
	}
	ref := ""
	if len(push.Commands) > 0 {
		c := push.Commands[0]
		ref = c.Old + " " + c.New + " " + c.Ref
	}
	// limited は、push.Raw の続き (pack) を、まだそのまま読める (ReadPush は、コマンド部の後を読まない)。
	body := io.MultiReader(bytes.NewReader(push.Raw), limited)
	h.relayGit(w, r, route, body, ref)
}

// relayGit は、route を上流へ中継する。body が nil なら GET (info/refs)、そうでなければ POST (upload-pack・receive-pack)。
// ref は、監査に残す "old new ref" (receive-pack のときだけ)。
func (h *Handler) relayGit(w http.ResponseWriter, r *http.Request, route git.Route, body io.Reader, ref ...string) {
	op := route.Op.String()
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	upReq, err := http.NewRequestWithContext(r.Context(), method, h.gitBase+route.Upstream(), body)
	if err != nil {
		h.rejectGit(w, route.Repo, op, http.StatusInternalServerError, "internal")
		return
	}
	if ct := route.Op.RequestType(); body != nil && ct != "" {
		upReq.Header.Set("Content-Type", ct)
	}
	if v := r.Header.Get("Accept"); v != "" {
		upReq.Header.Set("Accept", v)
	}
	if v := r.Header.Get("Git-Protocol"); v == "version=1" || v == "version=2" {
		upReq.Header.Set("Git-Protocol", v)
	}
	upReq.Header.Set("User-Agent", upstreamUserAgent)

	tok, err := h.creds.Token(r.Context(), CredentialName)
	if err != nil {
		h.rejectGit(w, route.Repo, op, http.StatusBadGateway, "credential")
		return
	}
	upReq.SetBasicAuth("x-access-token", tok.Reveal())

	start := time.Now()
	resp, err := h.client.Do(upReq)
	if err != nil {
		status, reason := http.StatusBadGateway, "upstream"
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) { // pack (body の続き) を送っている間に、上限を超えた
			status, reason = http.StatusRequestEntityTooLarge, "pack-too-large"
		}
		h.rejectGit(w, route.Repo, op, status, reason)
		return
	}
	defer resp.Body.Close()
	if tooLargeResponse(resp.ContentLength, h.maxPack) {
		h.rejectGit(w, route.Repo, op, http.StatusBadGateway, "response-too-large")
		return
	}
	for _, k := range []string{"Content-Type", "Cache-Control", "Vary"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, io.LimitReader(resp.Body, h.maxPack))
	rec := auditRecord{Event: "git", Repo: route.Repo.String(), Op: op, Status: resp.StatusCode, Bytes: n, Ms: time.Since(start).Milliseconds()}
	if len(ref) > 0 {
		rec.Ref = ref[0]
	}
	h.logAudit(rec)
}

// tooLargeResponse は、上流が Content-Length を返しているとき (cl > 0)、それが上限 (cap) を超えているか。
// 上流が Content-Length を返さない (chunked。cl <= 0) ときは、事前には分からないので false (io.LimitReader が、実際の
// 転送量を上限で切る)。
func tooLargeResponse(cl, cap int64) bool { return cl > 0 && cl > cap }

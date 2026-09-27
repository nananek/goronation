package gateway

import (
	"encoding/json"
	"time"

	"github.com/nananek/goronation/egress/git"
)

// auditRecord は、監査の 1 行 (JSON)。トークン・title・body は、決して入れない。
type auditRecord struct {
	Time   string `json:"time"`
	Event  string `json:"event"` // "git"・"pull"
	Repo   string `json:"repo,omitempty"`
	Op     string `json:"op,omitempty"`
	Ref    string `json:"ref,omitempty"` // receive-pack の最初のコマンド "old new ref"
	Status int    `json:"status"`
	Reason string `json:"reason,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Ms     int64  `json:"ms,omitempty"`
}

// logAudit は、rec を 1 行の JSON として、h.audit に書く。書き込みの失敗は無視する (監査が、中継を止めない)。
func (h *Handler) logAudit(rec auditRecord) {
	rec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	b = append(b, '\n')
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	h.audit.Write(b)
}

// repoString は、r が零値でなければ r.String()、そうでなければ空 (repo が特定できる前の拒否に使う)。
func repoString(r git.Repo) string {
	if r == (git.Repo{}) {
		return ""
	}
	return r.String()
}

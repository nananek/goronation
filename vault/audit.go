package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// auditEntry は、監査の記録の 1 行。秘密 (PRF・鍵・値) は、載せない。credential ID は、先頭の短い部分だけ。
type auditEntry struct {
	Time       string `json:"time"`
	Event      string `json:"event"`
	VaultID    string `json:"vault_id,omitempty"`
	Credential string `json:"credential,omitempty"`
	Target     string `json:"target,omitempty"`
	UID        int    `json:"uid"`
}

// idHead は、記録に載せる credential ID の先頭の長さ。
const idHead = 12

func head(id string) string {
	if len(id) > idHead {
		return id[:idHead]
	}
	return id
}

// appendAudit は、dir の audit.log に、1 行を追記する (0600・O_APPEND・1 回の write)。追記だけの通常の運用の記録で、同じ uid による
// 改ざん (切り詰め) の検知ではない。書けなければ error (呼び手は、操作を進めない)。
func appendAudit(dir string, now time.Time, e auditEntry) error {
	e.Time = now.UTC().Format(time.RFC3339Nano)
	e.UID = os.Geteuid()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(filepath.Join(dir, auditName), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("vault: 監査の記録を開けない: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("vault: 監査の記録を書けない: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("vault: 監査の記録を書けない: %w", err)
	}
	return f.Close()
}

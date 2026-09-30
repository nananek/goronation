package vault

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrVaultChanged は、Reset の ExpectID が、lock を取った後に読み直した ID と違うときの error (確認した Vault が、差し替わった)。
var ErrVaultChanged = errors.New("vault: the vault changed after confirmation")

// ResetOptions は、Reset の設定。
type ResetOptions struct {
	// Now は、監査の記録の時刻。nil なら time.Now。
	Now func() time.Time
	// ExpectID は、利用者が確認した Vault の ID (PeekID の値)。空でなければ、lock を取った直後に読み直した ID と一致しないとき、
	// 何も壊さずに ErrVaultChanged を返す (確認と実行の間の差し替え)。
	ExpectID string
}

// Reset は、dir の Vault の中身 (ラップ・暗号化した項目・Vault の ID) を壊す。PRF 出力・本体鍵・保管した値は、使わず、読まない
// (解錠していなくても実行できる)。web のログインの passkey や、監査の記録は、消さない。
//
// dir (ディレクトリ自身) の flock を取れなければ、生きた持ち主 (Vault のデーモン) が居るので、ErrInUse を返す。取れれば、持ち主は居ない。
// 壊す前に、監査の記録を追記する (書けなければ、壊さずに error)。消した Vault の ID を返す (読めなければ "unknown")。
// 対話の確認と、シェルからの実行の強制は、呼び手 (cmd) の責任。
func Reset(dir string, o ResetOptions) (vaultID string, err error) {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return "", ErrNotInitialized
	}
	if err := checkDir(dir); err != nil {
		return "", err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return "", err
	}
	defer lock.Close()

	vaultID = readVaultID(dir)
	if o.ExpectID != "" && o.ExpectID != vaultID {
		return "", ErrVaultChanged
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	if err := appendAudit(dir, now(), auditEntry{Event: "reset", VaultID: vaultID}); err != nil {
		return "", err
	}
	for _, name := range []string{fileName, tmpName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("vault: %s を消せない: %w", name, err)
		}
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return vaultID, nil
}

// readVaultID は、vault.json の公開の ID だけを、読めれば返す (読めなければ "unknown")。壊れた Vault でも、reset できるように、
// 検証しない (ID 以外は、読まない)。
func readVaultID(dir string) string {
	data, err := readRegular(dir, fileName, maxFileSize)
	if err != nil {
		return "unknown"
	}
	var v struct {
		ID []byte `json:"id"`
	}
	if json.Unmarshal(data, &v) != nil || len(v.ID) != vaultIDSize {
		return "unknown"
	}
	return hex.EncodeToString(v.ID)
}

// PeekID は、dir の Vault の公開の ID (16 進) を返す。reset の確認 (消す Vault を、利用者に見せる) に使う。dir が無ければ ErrNotInitialized、
// vault.json が無い・読めなければ "unknown"。値は、読まない。
func PeekID(dir string) (string, error) {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return "", ErrNotInitialized
	}
	if err := checkDir(dir); err != nil {
		return "", err
	}
	return readVaultID(dir), nil
}

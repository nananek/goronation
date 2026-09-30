package vault

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"unicode/utf8"
)

// ErrLocked は、Vault が施錠中 (未解錠) のときの error (errors.Is で判定する)。
var ErrLocked = errors.New("vault: locked")

// ErrNotFound は、名前のログイン状態が無いときの error (errors.Is で判定する)。
var ErrNotFound = errors.New("vault: not found")

// ErrInvalidState は、ログイン状態が上限や名前の規則に違反するときの error (errors.Is で判定する)。
var ErrInvalidState = errors.New("vault: invalid login state")

// ログイン状態 (LoginStore) の上限 (ADR 0032)。
const (
	// MaxStateFiles は、1 状態あたりのファイル数の上限。
	MaxStateFiles = 8
	// MaxStateFileSize は、1 ファイルの大きさの上限 (バイト)。
	MaxStateFileSize = 64 << 10
	// MaxStateTotalSize は、1 状態の合計の大きさの上限 (バイト)。
	MaxStateTotalSize = 256 << 10
	// MaxStateNameLen は、ファイル名の長さの上限 (バイト)。
	MaxStateNameLen = 128
)

// StateFile は、ログイン状態の 1 ファイル。名前は相対 path で、内容は檻の中のエージェントが書いたバイト列 (敵対入力)。
type StateFile struct {
	Name string
	Data []byte
}

// LoginStore は、エージェントのログイン状態の保管と復元の口。エージェント名 (credential.CheckName の形) で引く。
type LoginStore interface {
	// Put は、agent のログイン状態を保管する (置き換える)。上限・名前の規則に違反すれば ErrInvalidState を包んだ error を、
	// 施錠中なら ErrLocked を返す。
	Put(ctx context.Context, agent string, files []StateFile) error
	// Get は、agent のログイン状態を返す。無ければ ErrNotFound を、施錠中なら ErrLocked を返す。返す前に CheckFiles を通す。
	Get(ctx context.Context, agent string) ([]StateFile, error)
}

// Signer は、署名の口。鍵は返さない。実装は、gpg を動かす檻の形が決まる (M5) まで、置かない。
type Signer interface {
	// Sign は、keyID の鍵で data に署名して返す。施錠中なら ErrLocked を返す。
	Sign(ctx context.Context, keyID string, data []byte) ([]byte, error)
}

// CheckFiles は、files が上限と名前の規則を満たすか確かめる。違反すれば、ErrInvalidState を包んだ error を返す
// (error の文言に、内容を含めない)。Put の実装と、Get (復元) の前に、呼ぶ。
func CheckFiles(files []StateFile) error {
	if len(files) > MaxStateFiles {
		return fmt.Errorf("%w: ファイルが %d 個を超える", ErrInvalidState, MaxStateFiles)
	}
	seen := make(map[string]struct{}, len(files))
	total := 0
	for _, f := range files {
		if err := checkName(f.Name); err != nil {
			return err
		}
		if _, dup := seen[f.Name]; dup {
			return fmt.Errorf("%w: 名前が重複している", ErrInvalidState)
		}
		seen[f.Name] = struct{}{}
		if len(f.Data) > MaxStateFileSize {
			return fmt.Errorf("%w: 1 ファイルが %d バイトを超える", ErrInvalidState, MaxStateFileSize)
		}
		total += len(f.Data)
		if total > MaxStateTotalSize {
			return fmt.Errorf("%w: 合計が %d バイトを超える", ErrInvalidState, MaxStateTotalSize)
		}
	}
	return nil
}

// checkName は、名前が、相対・Clean 済み・128 バイト以内・制御文字なしの UTF-8 か確かめる。
func checkName(name string) error {
	switch {
	case name == "" || len(name) > MaxStateNameLen:
		return fmt.Errorf("%w: 名前の長さが不正", ErrInvalidState)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: 名前が UTF-8 でない", ErrInvalidState)
	case name == "." || !filepath.IsLocal(name) || filepath.Clean(name) != name:
		return fmt.Errorf("%w: 名前が相対の Clean な path でない", ErrInvalidState)
	}
	for _, r := range name {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return fmt.Errorf("%w: 名前に制御文字がある", ErrInvalidState)
		}
	}
	return nil
}

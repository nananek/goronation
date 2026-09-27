package webauthn

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nananek/goronation/cmd/internal/credfile"
	"github.com/nananek/goronation/core/credential"
)

// stateName は、credfile に保存する名前 ([a-z][a-z0-9-]{0,31} の形。credential.CheckName が確かめる)。
const stateName = "webauthn"

// storedCredential は、保存する登録済みの passkey (常に高々 1 つ)。
type storedCredential struct {
	ID         []byte `json:"id"`
	PublicKeyX []byte `json:"x"`
	PublicKeyY []byte `json:"y"`
	SignCount  uint32 `json:"sign_count"`
	RPID       string `json:"rp_id"` // 登録時の RP ID (今の設定と食い違えば、認証は自然に失敗する)
}

// persistedState は、credfile に JSON で保存する中身。[]byte のフィールドは、encoding/json が自動で
// base64 にする。
type persistedState struct {
	Credential      *storedCredential `json:"credential,omitempty"`
	SessionSecret   []byte            `json:"session_secret,omitempty"`
	BootstrapToken  []byte            `json:"bootstrap_token,omitempty"`
	BootstrapExpiry int64             `json:"bootstrap_expiry,omitempty"`
}

// Store は、webauthn の状態 (credential・セッション署名鍵・ブートストラップトークン) を、
// credfile.Store 経由でホストのファイルに保存する。
type Store struct {
	cf *credfile.Store
}

// NewStore は、状態ディレクトリ stateDir の Store を返す (credfile.New と同じ検証)。
func NewStore(stateDir string) (*Store, error) {
	cf, err := credfile.New(stateDir)
	if err != nil {
		return nil, err
	}
	return &Store{cf: cf}, nil
}

// load は、保存済みの状態を読む。まだ何も保存していなければ、ゼロ値 (Credential も SessionSecret も無い)。
func (s *Store) load(ctx context.Context) (persistedState, error) {
	sec, err := s.cf.Token(ctx, stateName)
	if errors.Is(err, credential.ErrNotFound) {
		return persistedState{}, nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("webauthn: 状態を読めない: %w", err)
	}
	var st persistedState
	if err := json.Unmarshal([]byte(sec.Reveal()), &st); err != nil {
		return persistedState{}, fmt.Errorf("webauthn: 保存された状態の形が壊れている: %w", err)
	}
	return st, nil
}

// save は、状態を原子的に保存する。
func (s *Store) save(st persistedState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("webauthn: 状態を組み立てられない: %w", err)
	}
	if err := s.cf.Save(stateName, credential.New(string(b))); err != nil {
		return fmt.Errorf("webauthn: 状態を保存できない: %w", err)
	}
	return nil
}

// randomBytes は、n バイトの乱数。
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("webauthn: 乱数を得られない: %w", err)
	}
	return b, nil
}

// ensureSessionSecret は、st.SessionSecret が無ければ、新しく作る (呼び手が、変更後に save する)。
func ensureSessionSecret(st *persistedState) error {
	if len(st.SessionSecret) > 0 {
		return nil
	}
	secret, err := randomBytes(32)
	if err != nil {
		return err
	}
	st.SessionSecret = secret
	return nil
}

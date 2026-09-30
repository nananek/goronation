package webauthn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nananek/goronation/cmd/internal/credfile"
	"github.com/nananek/goronation/core/credential"
)

// maxStateValue は、保存する状態 (JSON) の大きさの上限 (バイト)。passkey 16 個 (credential ID 128 バイト・label 32 文字まで) が収まる。
// credfile の既定 (1 KiB) では、2 つ目の passkey から収まらない。
const maxStateValue = 16 << 10

// maxCredentialIDBytes は、登録を受け付ける credential ID の大きさ (バイト)。WebAuthn の上限は 1023 だが、実機の passkey は数十バイト。
const maxCredentialIDBytes = 128

// stateName は、credfile に保存する名前 ([a-z][a-z0-9-]{0,31} の形。credential.CheckName が確かめる)。
const stateName = "webauthn"

// maxCredentials は、登録できる passkey の数の上限 (Vault のラップの上限と同じ。ADR 0038)。
const maxCredentials = 16

// storedCredential は、保存する登録済みの passkey。
type storedCredential struct {
	ID         []byte `json:"id"`
	PublicKeyX []byte `json:"x"`
	PublicKeyY []byte `json:"y"`
	SignCount  uint32 `json:"sign_count"`
	RPID       string `json:"rp_id"`           // 登録時の RP ID (今の設定と食い違えば、認証は自然に失敗する)
	Label      []byte `json:"label,omitempty"` // UTF-8 (base64 で保存する: credfile の値は、印字できる ASCII 1 語だけ)
	CreatedAt  int64  `json:"created_at,omitempty"`
}

// persistedState は、credfile に JSON で保存する中身。[]byte のフィールドは、encoding/json が自動で
// base64 にする。
type persistedState struct {
	// Credential は、単数だった版の保存形式 (読むときだけ使う)。load が Credentials に移し、save は書かない。
	Credential      *storedCredential  `json:"credential,omitempty"`
	Credentials     []storedCredential `json:"credentials,omitempty"`
	SessionSecret   []byte             `json:"session_secret,omitempty"`
	BootstrapToken  []byte             `json:"bootstrap_token,omitempty"`
	BootstrapExpiry int64              `json:"bootstrap_expiry,omitempty"`
	// SessionEpoch は、発行済みのセッション token を一括で失効させるための世代番号。ログアウトのたびに
	// 進める。セッション token は発行時点の世代を埋め込み、検証時にここと一致しないものは拒否する。
	SessionEpoch int64 `json:"session_epoch,omitempty"`
}

// Store は、webauthn の状態 (credential・セッション署名鍵・ブートストラップトークン) を、
// credfile.Store 経由でホストのファイルに保存する。
type Store struct {
	cf *credfile.Store
	// mu は、読んで書き換えて保存する操作 (passkey の追加・削除・signCount の更新) を、直列にする。
	mu sync.Mutex
	// used は、使用済みの操作つきの認証 (OpAuthFinish) の challenge (期限まで)。同じ応答の再送を断る。プロセスの中だけ。
	used map[string]int64
}

// NewStore は、状態ディレクトリ stateDir の Store を返す (credfile.New と同じ検証)。
func NewStore(stateDir string) (*Store, error) {
	cf, err := credfile.NewSized(stateDir, maxStateValue)
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
	if st.Credential != nil && len(st.Credentials) == 0 { // 単数だった版から移す
		st.Credentials = []storedCredential{*st.Credential}
	}
	st.Credential = nil
	if len(st.Credentials) > maxCredentials {
		return persistedState{}, errors.New("webauthn: 保存された passkey の数が上限を超えている")
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

// credential は、id の passkey を返す (無ければ nil)。返すのは、st の中を指す。
func (st *persistedState) credential(id []byte) *storedCredential {
	for i := range st.Credentials {
		if bytes.Equal(st.Credentials[i].ID, id) {
			return &st.Credentials[i]
		}
	}
	return nil
}

// consume は、key (challenge) を使用済みにする。すでに使用済みなら false。expiry (unix 秒) を過ぎたものは、ここで忘れる。
func (s *Store) consume(key string, expiry int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumeLocked(key, expiry)
}

// consumeLocked は、s.mu を持っている呼び手用の consume。
func (s *Store) consumeLocked(key string, expiry int64) bool {
	now := time.Now().Unix()
	for k, exp := range s.used {
		if exp < now {
			delete(s.used, k)
		}
	}
	if _, dup := s.used[key]; dup {
		return false
	}
	if s.used == nil {
		s.used = map[string]int64{}
	}
	s.used[key] = expiry
	return true
}

package vault

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/nananek/goronation/core/credential"
	corevault "github.com/nananek/goronation/core/vault"
)

// 状態の error (errors.Is で判定する)。
var (
	// ErrLocked は、施錠中の操作の error。core/vault の ErrLocked と同じ値。
	ErrLocked = corevault.ErrLocked
	// ErrNotInitialized は、Vault が未初期化 (ラップが 1 つも無い) のときの error。
	ErrNotInitialized = errors.New("vault: not initialized")
	// ErrAlreadyInitialized は、初期化済みの Vault を、Init しようとしたときの error。
	ErrAlreadyInitialized = errors.New("vault: already initialized")
	// ErrUnlockFailed は、PRF で本体鍵を復号できないときの error。credential ID が未登録・PRF が違う・データが壊れている、を区別しない。
	ErrUnlockFailed = errors.New("vault: unlock failed")
	// ErrWrapExists は、同じ credential ID のラップが、既にあるときの error (上書きしない)。
	ErrWrapExists = errors.New("vault: wrap already exists")
	// ErrWrapNotFound は、削除する credential ID のラップが無いときの error。
	ErrWrapNotFound = errors.New("vault: wrap not found")
	// ErrLastWrap は、最後の 1 つのラップを消そうとしたときの error。
	ErrLastWrap = errors.New("vault: cannot remove the last wrap")
	// ErrSameCredential は、削除する passkey 自身を、認可に使おうとしたときの error。
	ErrSameCredential = errors.New("vault: authorizer must differ from the removed passkey")
	// ErrInUse は、別の持ち主が、Vault の lock (flock) を持っているときの error。
	ErrInUse = errors.New("vault: in use by another process")
	// ErrClosed は、Close 済みの Vault を使ったときの error。
	ErrClosed = errors.New("vault: closed")
)

// 保管する値の上限。
const (
	maxSecretSize = 16 << 10 // 資格情報 1 つ
	maxItemSize   = 1 << 20  // 暗号化前の項目 1 つ
)

// Proof は、既存の passkey (authorizer) の証明。PRF 出力が、自分のラップを、実際に復号できることを見る (ADR 0038)。
type Proof struct {
	CredentialID string
	PRF          []byte
}

// Enrollment は、追加する passkey (candidate)。Salt は、Vault のデーモンが要求の始めに作った、PRF に渡した salt。PRF は、検証できない
// (署名の外) ので、認可には使わず、ラップを作るのにだけ使う。
type Enrollment struct {
	CredentialID string
	Salt         []byte
	PRF          []byte
}

// WrapInfo は、passkey のラップの公開の情報。認証の options の evalByCredential に、Salt を渡す。
type WrapInfo struct {
	CredentialID string
	Salt         []byte
}

// Vault は、Vault の本体。1 つの dir を、flock で占有する。全ての method は、並行に呼んでよい。
//
// 呼び手が渡した PRF 出力・salt の slice は、Vault は保持しない (呼び手が clear する)。
type Vault struct {
	mu     sync.Mutex
	dir    string
	lock   *os.File
	doc    *document // nil なら未初期化
	key    []byte    // 本体鍵。nil なら施錠中
	closed bool
	now    func() time.Time
}

var (
	_ credential.Source    = (*Vault)(nil)
	_ corevault.LoginStore = (*Vault)(nil)
)

// Open は、dir の Vault を開く。dir は、自分の持つ 0700 の実ディレクトリ (無ければ作る)。lock (flock) を取れなければ ErrInUse。
// vault.json が無ければ、未初期化の Vault を返す。読めない・版が違う vault.json は ErrFormat (Reset で壊せる)。
// 開いた直後は、施錠中。
func Open(dir string) (*Vault, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	doc, err := loadDocument(dir)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Vault{dir: dir, lock: lock, doc: doc, now: time.Now}, nil
}

// Close は、施錠して、lock を手放す。
func (v *Vault) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.lockLocked()
	v.closed = true
	return v.lock.Close()
}

func (v *Vault) lockLocked() {
	clear(v.key)
	v.key = nil
}

// Lock は、本体鍵をメモリから消す (施錠する)。
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lockLocked()
}

// Unlocked は、解錠中か。
func (v *Vault) Unlocked() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.key != nil
}

// Initialized は、Vault が初期化済みか。
func (v *Vault) Initialized() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.doc != nil
}

// NewSalt は、passkey ごとの PRF の salt (SaltSize バイトの乱数) を作る。認証 (Init・AddWrap) の前に、呼び手がブラウザへ渡す。
func NewSalt() ([]byte, error) { return randBytes(SaltSize) }

// WrapInfos は、ラップの公開の情報 (credential ID と salt) を返す。解錠の認証の evalByCredential に使う。
func (v *Vault) WrapInfos() []WrapInfo {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.doc == nil {
		return nil
	}
	out := make([]WrapInfo, 0, len(v.doc.Wraps))
	for _, w := range v.doc.Wraps {
		out = append(out, WrapInfo{CredentialID: w.CredentialID, Salt: slices.Clone(w.Salt)})
	}
	return out
}

func (v *Vault) check() error {
	if v.closed {
		return ErrClosed
	}
	return nil
}

func checkID(id string) error {
	if !validID(id) {
		return fmt.Errorf("%w: credential ID の形が不正", ErrInvalidInput)
	}
	return nil
}

func checkSized(name string, b []byte, n int) error {
	if len(b) != n {
		return fmt.Errorf("%w: %s は %d バイト", ErrInvalidInput, name, n)
	}
	return nil
}

// Init は、未初期化の Vault に、新しい ID・本体鍵を作り、passkey のラップを最初の 1 つにする。終わると、解錠中になる。
// 呼び手は、credentialID が web のログインに登録済みの passkey であること (ADR 0034・0038) と、salt が NewSalt の値で、
// PRF がその salt で評価した出力であることを保証する (Vault は、どちらも検証できない)。
func (v *Vault) Init(credentialID string, salt, prf []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.check(); err != nil {
		return err
	}
	if v.doc != nil {
		return ErrAlreadyInitialized
	}
	if err := errors.Join(checkID(credentialID), checkSized("salt", salt, SaltSize), checkSized("PRF", prf, PRFSize)); err != nil {
		return err
	}
	id, err := randBytes(vaultIDSize)
	if err != nil {
		return err
	}
	bk, err := randBytes(keySize)
	if err != nil {
		return err
	}
	nonce, ct, err := wrapBodyKey(bk, prf, id, credentialID, salt)
	if err != nil {
		clear(bk)
		return err
	}
	doc := &document{
		Version: formatVersion, ID: id,
		Wraps: []wrapRecord{{CredentialID: credentialID, Salt: slices.Clone(salt), Nonce: nonce, Ct: ct}},
	}
	if err := v.commit(doc, auditEntry{Event: "init", VaultID: hex.EncodeToString(id), Credential: head(credentialID)}); err != nil {
		clear(bk)
		return err
	}
	v.key = bk
	return nil
}

// find は、credentialID のラップを返す。
func (v *Vault) find(credentialID string) (wrapRecord, bool) {
	for _, w := range v.doc.Wraps {
		if w.CredentialID == credentialID {
			return w, true
		}
	}
	return wrapRecord{}, false
}

// prove は、p の PRF が、p の passkey のラップを、実際に復号できることを確かめ、本体鍵を返す (呼び手が clear する)。
// 未登録・PRF 違い・壊れたデータは、ErrUnlockFailed (区別しない)。保存内容は、変えない。
func (v *Vault) prove(p Proof) ([]byte, error) {
	if err := errors.Join(checkID(p.CredentialID), checkSized("PRF", p.PRF, PRFSize)); err != nil {
		return nil, err
	}
	w, ok := v.find(p.CredentialID)
	if !ok {
		return nil, ErrUnlockFailed
	}
	bk, err := unwrapBodyKey(p.PRF, v.doc.ID, w.CredentialID, w.Salt, w.Nonce, w.Ct)
	if err != nil {
		return nil, ErrUnlockFailed
	}
	return bk, nil
}

// Unlock は、passkey の PRF 出力で本体鍵を復号し、解錠する。PRF が合わなくても、保存内容は変わらない (置き換えない)。
// PRF は、検証できない値で、復号を試す入力にだけ使う (ADR 0033)。
func (v *Vault) Unlock(credentialID string, prf []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.check(); err != nil {
		return err
	}
	if v.doc == nil {
		return ErrNotInitialized
	}
	bk, err := v.prove(Proof{CredentialID: credentialID, PRF: prf})
	if err != nil {
		return err
	}
	clear(v.key)
	v.key = bk
	return nil
}

// AddWrap は、passkey のラップを追加する (追加だけ。ラップを変える 2 か所の 1 つ。ADR 0038)。
//
// 認可は、authorizer の PRF が、自分のラップを実際に復号できることだけ (解錠中かどうかは見ない。施錠中でも追加できる)。
// candidate の PRF は、検証できないので、認可に使わない。同じ credential ID のラップが既にあれば、ErrWrapExists
// (上書きしない)。操作の確認 (SAS)・challenge の束縛は、呼び手の責任。追加は、監査の記録に残す (書けなければ、追加しない)。
func (v *Vault) AddWrap(authorizer Proof, candidate Enrollment) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.check(); err != nil {
		return err
	}
	if v.doc == nil {
		return ErrNotInitialized
	}
	if err := errors.Join(checkID(candidate.CredentialID), checkSized("salt", candidate.Salt, SaltSize), checkSized("PRF", candidate.PRF, PRFSize)); err != nil {
		return err
	}
	bk, err := v.prove(authorizer)
	if err != nil {
		v.auditDenied("add-wrap-denied", authorizer.CredentialID, candidate.CredentialID)
		return err
	}
	defer clear(bk)
	if _, exists := v.find(candidate.CredentialID); exists {
		return ErrWrapExists
	}
	if len(v.doc.Wraps) >= maxWraps {
		return fmt.Errorf("%w: ラップが %d 個を超える", ErrInvalidInput, maxWraps)
	}
	nonce, ct, err := wrapBodyKey(bk, candidate.PRF, v.doc.ID, candidate.CredentialID, candidate.Salt)
	if err != nil {
		return err
	}
	next := v.cloneDoc()
	next.Wraps = append(next.Wraps, wrapRecord{CredentialID: candidate.CredentialID, Salt: slices.Clone(candidate.Salt), Nonce: nonce, Ct: ct})
	return v.commit(next, auditEntry{Event: "add-wrap", VaultID: hex.EncodeToString(v.doc.ID), Credential: head(authorizer.CredentialID), Target: head(candidate.CredentialID)})
}

// RemoveWrap は、passkey のラップを削除する (ラップを変える 2 か所の 1 つ。ADR 0038)。
//
// 条件: 解錠済み (施錠中は ErrLocked)・target とは別の passkey (authorizer) の証明 (ErrSameCredential・ErrUnlockFailed)・
// target が在る (ErrWrapNotFound)・最後の 1 つではない (ErrLastWrap)。削除は将来の解錠だけを止める (既に本体鍵を得た者は、持ち続ける。
// 全部の失効は Reset)。操作の確認 (SAS) は、呼び手の責任。監査の記録に残す (書けなければ、削除しない)。
func (v *Vault) RemoveWrap(authorizer Proof, target string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.check(); err != nil {
		return err
	}
	if v.doc == nil {
		return ErrNotInitialized
	}
	if v.key == nil {
		return ErrLocked
	}
	if err := checkID(target); err != nil {
		return err
	}
	if authorizer.CredentialID == target {
		return ErrSameCredential
	}
	bk, err := v.prove(authorizer)
	if err != nil {
		v.auditDenied("remove-wrap-denied", authorizer.CredentialID, target)
		return err
	}
	defer clear(bk)
	if subtle.ConstantTimeCompare(bk, v.key) != 1 { // 同じ Vault の本体鍵であること (壊れたデータへの防御)
		return ErrUnlockFailed
	}
	if len(v.doc.Wraps) <= 1 {
		return ErrLastWrap
	}
	if _, ok := v.find(target); !ok {
		return ErrWrapNotFound
	}
	next := v.cloneDoc()
	next.Wraps = slices.DeleteFunc(next.Wraps, func(w wrapRecord) bool { return w.CredentialID == target })
	return v.commit(next, auditEntry{Event: "remove-wrap", VaultID: hex.EncodeToString(v.doc.ID), Credential: head(authorizer.CredentialID), Target: head(target)})
}

// auditDenied は、認可に失敗した追加・削除を、記録する (書けなくても、失敗の返り値は変えない)。
func (v *Vault) auditDenied(event, authorizer, target string) {
	if validID(authorizer) && validID(target) {
		_ = appendAudit(v.dir, v.now(), auditEntry{Event: event, VaultID: hex.EncodeToString(v.doc.ID), Credential: head(authorizer), Target: head(target)})
	}
}

func (v *Vault) cloneDoc() *document {
	d := *v.doc
	d.Wraps = slices.Clone(v.doc.Wraps)
	d.Items = slices.Clone(v.doc.Items)
	return &d
}

// write は、next を vault.json に書く。ディスクが新しい内容になった (rename 済み) なら、error (ErrNotDurable) でも、
// メモリを next に合わせる (メモリとディスクを、常に一致させる。古い v.doc の書き戻しで、削除したラップを復活させない)。
func (v *Vault) write(next *document) (applied bool, err error) {
	applied, err = writeDocument(v.dir, next)
	if applied {
		v.doc = next
	}
	return applied, err
}

// commit は、監査の記録を先に追記し (書けなければ、変更しない)、next を書いて、メモリの状態を置き換える。
// 書けなければ "-failed"、適用済みで永続化が未確認なら "-not-durable" を、記録する (適用済みなのに、失敗と記録しない)。
func (v *Vault) commit(next *document, e auditEntry) error {
	if err := appendAudit(v.dir, v.now(), e); err != nil {
		return err
	}
	if applied, err := v.write(next); err != nil {
		suffix := "-failed"
		if applied {
			suffix = "-not-durable"
		}
		_ = appendAudit(v.dir, v.now(), auditEntry{Event: e.Event + suffix, VaultID: e.VaultID, Credential: e.Credential, Target: e.Target})
		return err
	}
	return nil
}

// putItem は、項目を暗号化して置き換える (書き込みごとに、新しい乱数で鍵が変わる)。
func (v *Vault) putItem(kind, name string, plaintext []byte) error {
	if err := v.check(); err != nil {
		return err
	}
	if v.doc == nil {
		return ErrNotInitialized
	}
	if v.key == nil {
		return ErrLocked
	}
	if len(plaintext) > maxItemSize {
		return fmt.Errorf("%w: 項目が大きすぎる", ErrInvalidInput)
	}
	rnd, ct, err := sealItem(v.key, v.doc.ID, kind, name, plaintext)
	if err != nil {
		return err
	}
	next := v.cloneDoc()
	rec := itemRecord{Kind: kind, Name: name, Rand: rnd, Ct: ct}
	if i := slices.IndexFunc(next.Items, func(it itemRecord) bool { return it.Kind == kind && it.Name == name }); i >= 0 {
		next.Items[i] = rec
	} else {
		if len(next.Items) >= maxItems {
			return fmt.Errorf("%w: 項目が %d 個を超える", ErrInvalidInput, maxItems)
		}
		next.Items = append(next.Items, rec)
	}
	_, err = v.write(next)
	return err
}

// getItem は、項目を復号して返す (呼び手が clear する)。無ければ (nil, false, nil)。
func (v *Vault) getItem(kind, name string) ([]byte, bool, error) {
	if err := v.check(); err != nil {
		return nil, false, err
	}
	if v.doc == nil {
		return nil, false, ErrNotInitialized
	}
	if v.key == nil {
		return nil, false, ErrLocked
	}
	for _, it := range v.doc.Items {
		if it.Kind == kind && it.Name == name {
			pt, err := openItem(v.key, v.doc.ID, kind, name, it.Rand, it.Ct)
			if err != nil {
				return nil, false, fmt.Errorf("%w: 項目を復号できない", ErrFormat)
			}
			return pt, true, nil
		}
	}
	return nil, false, nil
}

// PutCredential は、資格情報 (名前は credential.CheckName の形・値は 16 KiB まで) を保管する (置き換える)。施錠中は ErrLocked。
func (v *Vault) PutCredential(name string, secret credential.Secret) error {
	if err := credential.CheckName(name); err != nil {
		return err
	}
	val := []byte(secret.Reveal())
	defer clear(val)
	if len(val) == 0 || len(val) > maxSecretSize {
		return fmt.Errorf("%w: 資格情報の大きさが不正", ErrInvalidInput)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.putItem(kindCredential, name, val)
}

// Token は、name の資格情報を返す (credential.Source)。施錠中は ErrLocked、無ければ credential.ErrNotFound。
func (v *Vault) Token(_ context.Context, name string) (credential.Secret, error) {
	if err := credential.CheckName(name); err != nil {
		return credential.Secret{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	pt, ok, err := v.getItem(kindCredential, name)
	if err != nil {
		return credential.Secret{}, err
	}
	if !ok {
		return credential.Secret{}, credential.ErrNotFound
	}
	s := credential.New(string(pt))
	clear(pt)
	return s, nil
}

// loginState は、ログイン状態の、暗号化前の形。
type loginState struct {
	Files []corevault.StateFile `json:"files"`
}

// Put は、agent のログイン状態を保管する (置き換える。corevault.LoginStore)。内容は、檻が書いた敵対入力として、上限を検査する。
func (v *Vault) Put(_ context.Context, agent string, files []corevault.StateFile) error {
	if err := credential.CheckName(agent); err != nil {
		return err
	}
	if err := corevault.CheckFiles(files); err != nil {
		return err
	}
	pt, err := json.Marshal(loginState{Files: files})
	if err != nil {
		return err
	}
	defer clear(pt)
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.putItem(kindLoginState, agent, pt)
}

// Get は、agent のログイン状態を返す (corevault.LoginStore)。返す前に、上限を検査する。無ければ corevault.ErrNotFound。
func (v *Vault) Get(_ context.Context, agent string) ([]corevault.StateFile, error) {
	if err := credential.CheckName(agent); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	pt, ok, err := v.getItem(kindLoginState, agent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, corevault.ErrNotFound
	}
	defer clear(pt)
	var st loginState
	if err := json.Unmarshal(pt, &st); err != nil {
		return nil, fmt.Errorf("%w: ログイン状態を読めない", ErrFormat)
	}
	if err := corevault.CheckFiles(st.Files); err != nil {
		return nil, err
	}
	return st.Files, nil
}

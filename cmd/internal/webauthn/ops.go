package webauthn

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// Op は、操作つきの儀式の種類 (ADR 0038 決定 5。challenge を、操作の種類・対象の credential ID・要求の ID に束縛する)。
type Op string

// 操作の種類。
const (
	OpUnlock           Op = "unlock"            // Vault の解錠 (対象なし)
	OpAddCredential    Op = "add-credential"    // passkey の追加 (対象は、追加する passkey)
	OpRemoveCredential Op = "remove-credential" // passkey の削除 (対象は、消す passkey)
)

// Binding は、儀式が束縛される操作。呼び手 (web) は、自分の要求の表から、Begin と Finish の両方に、同じ値を渡す。
// Finish は、state token に埋まった値と、渡された値が、食い違えば断る (ある要求への認証で、別の要求・別の対象は通らない)。
type Binding struct {
	Op        Op
	Target    []byte // OpAddCredential・OpRemoveCredential だけ (OpUnlock では空)
	RequestID string // 呼び手が、要求ごとに作る、1 回限りの ID。[A-Za-z0-9_-]{1,64}
}

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (b Binding) validate() error {
	if !requestIDRe.MatchString(b.RequestID) {
		return errors.New("webauthn: 要求の ID の形が不正")
	}
	switch b.Op {
	case OpUnlock:
		if len(b.Target) != 0 {
			return errors.New("webauthn: 解錠に、対象は付けられない")
		}
	case OpAddCredential, OpRemoveCredential:
		if len(b.Target) == 0 || len(b.Target) > maxCredentialIDBytes {
			return errors.New("webauthn: 対象の credential ID が不正")
		}
	default:
		return fmt.Errorf("webauthn: 未知の操作 %q", b.Op)
	}
	return nil
}

func (b Binding) equal(op string, target []byte, requestID string) bool {
	return string(b.Op) == op && bytes.Equal(b.Target, target) && b.RequestID == requestID
}

// PRFEval は、認証時に、ある passkey へ渡す PRF の salt (Vault の WrapInfos の値)。
type PRFEval struct {
	CredentialID []byte
	Salt         []byte
}

// ErrBadOperation は、操作の前提 (対象の有無・最後の 1 つ・認可の種類) が合わないときの error。
var ErrBadOperation = errors.New("webauthn: 操作を行えない")

// OpAuthBegin は、登録済みの passkey による、操作つきの再認証 (UV 必須) の PublicKeyCredentialRequestOptions と、state token を返す。
//
// evals があれば、PRF の評価 (evalByCredential) を要求し、応えてよい passkey を、evals にある (登録済みの) ものだけにする。
// evals が空なら、PRF を要求しない (passkey のログインの追加・削除の認可だけ)。OpRemoveCredential では、消す passkey 自身は、
// 認可に使えない。OpAddCredential の対象は、まだ登録されていない (AddFinish が返した Candidate の ID)。
func OpAuthBegin(ctx context.Context, cfg Config, st *Store, b Binding, evals []PRFEval) (*RequestOptions, string, error) {
	if err := b.validate(); err != nil {
		return nil, "", err
	}
	state, err := st.load(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(state.Credentials) == 0 {
		return nil, "", ErrNotRegistered
	}
	if err := checkOpTarget(state, b); err != nil {
		return nil, "", err
	}
	var except []byte
	if b.Op == OpRemoveCredential {
		except = b.Target
	}
	allowed := state.Credentials
	var ext *requestExtensions
	if len(evals) > 0 {
		ext = &requestExtensions{}
		ext.PRF.EvalByCredential = map[string]prfSalt{}
		allowed = nil
		for _, e := range evals {
			cred := state.credential(e.CredentialID)
			if cred == nil || len(e.Salt) != PRFSize {
				return nil, "", errors.New("webauthn: PRF の評価の対象が、登録済みの passkey でない (または salt の長さが不正)")
			}
			key := b64.EncodeToString(e.CredentialID)
			if _, dup := ext.PRF.EvalByCredential[key]; dup {
				return nil, "", errors.New("webauthn: PRF の評価の対象が重複している")
			}
			ext.PRF.EvalByCredential[key] = prfSalt{First: b64.EncodeToString(e.Salt)}
			if except == nil || !bytes.Equal(e.CredentialID, except) {
				allowed = append(allowed, *cred)
			}
		}
	}
	descs := descriptors(allowed, except)
	if len(descs) == 0 {
		return nil, "", fmt.Errorf("%w: 認可に使える passkey が無い", ErrBadOperation)
	}
	challenge, err := randomBytes(32)
	if err != nil {
		return nil, "", err
	}
	claims := stateClaims{
		Purpose: "op-auth", Challenge: challenge, Expiry: time.Now().Add(challengeTTL).Unix(),
		Op: string(b.Op), Target: b.Target, RequestID: b.RequestID, WantPRF: len(evals) > 0,
	}
	for _, c := range allowed {
		if except == nil || !bytes.Equal(c.ID, except) {
			claims.Allowed = append(claims.Allowed, c.ID)
		}
	}
	opts := &RequestOptions{
		RPID: cfg.RPID, Challenge: b64.EncodeToString(challenge), AllowCredentials: descs,
		Timeout: int(challengeTTL / time.Millisecond), UserVerification: "required", Extensions: ext,
	}
	return opts, signToken(state.SessionSecret, mustJSON(claims)), nil
}

// checkOpTarget は、操作の対象の前提を確かめる (追加: 未登録・数の上限。削除: 登録済み・最後の 1 つでない)。
func checkOpTarget(state persistedState, b Binding) error {
	switch b.Op {
	case OpAddCredential:
		if state.credential(b.Target) != nil {
			return fmt.Errorf("%w: すでに登録済みの passkey", ErrBadOperation)
		}
		if len(state.Credentials) >= maxCredentials {
			return fmt.Errorf("%w: passkey が %d 個を超える", ErrBadOperation, maxCredentials)
		}
	case OpRemoveCredential:
		if state.credential(b.Target) == nil {
			return fmt.Errorf("%w: 対象の passkey が無い", ErrBadOperation)
		}
		if len(state.Credentials) <= 1 {
			return fmt.Errorf("%w: 最後の 1 つは消せない", ErrBadOperation)
		}
	}
	return nil
}

// Assertion は、OpAuthFinish が通した、操作つきの認証の結果 (検証済みの証明)。CommitAdd・RemoveCredential の認可に使う。
// 値は、OpAuthFinish だけが作れる (フィールドは非公開)。PRF は、検証できない値 (署名の外。ADR 0033 決定 5): 呼び手は、
// 復号を試す入力にだけ使い、使い終えたら Wipe する。
type Assertion struct {
	// CredentialID は、認証に使った passkey。
	CredentialID []byte
	// PRF は、passkey が返した PRF の出力 (PRFSize バイト)。OpAuthBegin に evals を渡したときだけ。
	PRF []byte

	cred      []byte // CredentialID の、非公開の写し (認可の検査は、こちらを見る)
	op        string
	target    []byte
	requestID string
	at        time.Time
}

// Wipe は、PRF を消す。
func (a *Assertion) Wipe() {
	clear(a.PRF)
	a.PRF = nil
}

// OpAuthFinish は、state と応答を検証する。b は、state に埋まった束縛と一致すること (呼び手の要求の表から渡す)。応答の passkey は、
// OpAuthBegin が許したものだけ。UV 必須。同じ応答 (challenge) は、1 回しか通らない (期限まで、プロセスの中で覚える)。
// evals を渡していた場合、PRF の出力 (PRFSize バイト) が無ければ error。セッション token は、発行しない。
func OpAuthFinish(ctx context.Context, cfg Config, st *Store, state string, b Binding, resp AssertionResponse) (*Assertion, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	persisted, err := st.load(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := verifyStateToken(persisted.SessionSecret, state, "op-auth")
	if err != nil {
		return nil, err
	}
	if !b.equal(claims.Op, claims.Target, claims.RequestID) {
		return nil, errors.New("webauthn: この認証は、別の操作・対象・要求のもの")
	}
	respID, err := b64.DecodeString(resp.ID)
	if err != nil {
		return nil, errors.New("webauthn: 応答の id が不正")
	}
	cred := persisted.credential(respID)
	if cred == nil || !containsID(claims.Allowed, respID) {
		return nil, errors.New("webauthn: この認証に応えてよい passkey でない")
	}
	if err := checkOpTarget(persisted, b); err != nil {
		return nil, err
	}
	authData, err := verifyAssertion(cfg, cred, claims.Challenge, resp, true)
	if err != nil {
		return nil, err
	}
	var prf []byte
	if claims.WantPRF {
		if prf, err = resp.ClientExtensionResults.prfFirst(); err != nil {
			return nil, err
		}
		if prf == nil {
			return nil, errors.New("webauthn: PRF の出力が無い")
		}
	}
	if !st.consumeLocked(b64.EncodeToString(claims.Challenge), claims.Expiry) {
		clear(prf)
		return nil, errors.New("webauthn: この認証は、すでに使われた")
	}
	if authData.SignCount > 0 {
		cred.SignCount = authData.SignCount
		if err := st.save(persisted); err != nil {
			clear(prf)
			return nil, err
		}
	}
	return &Assertion{
		CredentialID: bytes.Clone(respID), PRF: prf, cred: bytes.Clone(respID),
		op: claims.Op, target: bytes.Clone(claims.Target), requestID: claims.RequestID, at: time.Now(),
	}, nil
}

func containsID(ids [][]byte, id []byte) bool {
	for _, x := range ids {
		if subtle.ConstantTimeCompare(x, id) == 1 {
			return true
		}
	}
	return false
}

// Candidate は、AddFinish が検証した、追加する passkey (まだ保存していない)。CommitAdd で、認可 (Assertion) と合わせて保存する。
type Candidate struct {
	// CredentialID・Salt・PRF は、vault.Enrollment に渡す値。Salt は、AddBegin が作った、この passkey の PRF の salt。
	CredentialID []byte
	Salt         []byte
	// PRF は、登録時の clientExtensionResults の prf.results.first (無ければ nil)。検証できない値 (署名の外)。
	// 多くの実装で、作成時には返らない (S9 で、iPhone の実機を確かめる)。nil のとき、Vault のラップを作るには、
	// 追加した passkey での認証 (evalByCredential) が、別に要る (この package の範囲外。後続の PR)。
	PRF []byte
	// PRFEnabled は、登録時の prf.enabled (この passkey が PRF に対応するか)。
	PRFEnabled bool

	cred      storedCredential
	requestID string
	at        time.Time
}

// Wipe は、PRF を消す。
func (c *Candidate) Wipe() {
	clear(c.PRF)
	c.PRF = nil
}

// AddBegin は、passkey の追加の、登録の PublicKeyCredentialCreationOptions (すでにある passkey は excludeCredentials・UV 必須・
// PRF 用の新しい salt つき) と、state token を返す。追加には、ブートストラップトークンは要らない (代わりに、CommitAdd が、
// 既存の passkey の認可を要る)。requestID は、呼び手の要求の ID。
func AddBegin(ctx context.Context, cfg Config, st *Store, requestID string) (*CreationOptions, string, error) {
	if !requestIDRe.MatchString(requestID) {
		return nil, "", errors.New("webauthn: 要求の ID の形が不正")
	}
	state, err := st.load(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(state.Credentials) == 0 {
		return nil, "", ErrNotRegistered
	}
	if len(state.Credentials) >= maxCredentials {
		return nil, "", fmt.Errorf("%w: passkey が %d 個を超える", ErrBadOperation, maxCredentials)
	}
	challenge, err := randomBytes(32)
	if err != nil {
		return nil, "", err
	}
	salt, err := randomBytes(PRFSize)
	if err != nil {
		return nil, "", err
	}
	opts := &CreationOptions{
		RP:                     rpEntity{ID: cfg.RPID, Name: cfg.RPName},
		User:                   userEntity{ID: b64.EncodeToString(bootstrapUserID), Name: "admin", DisplayName: "goronation"},
		Challenge:              b64.EncodeToString(challenge),
		PubKeyCredParams:       []credParam{{Type: "public-key", Alg: coseAlgES256}},
		Timeout:                int(challengeTTL / time.Millisecond),
		Attestation:            "none",
		AuthenticatorSelection: authenticatorSelection{ResidentKey: "preferred", UserVerification: "required"},
		ExcludeCredentials:     descriptors(state.Credentials, nil),
		Extensions:             &creationExtensions{PRF: prfCreate{Eval: &prfSalt{First: b64.EncodeToString(salt)}}},
	}
	token := signToken(state.SessionSecret, mustJSON(stateClaims{
		Purpose: "add-register", Challenge: challenge, Expiry: time.Now().Add(challengeTTL).Unix(),
		RequestID: requestID, Salt: salt,
	}))
	return opts, token, nil
}

// AddFinish は、state と登録の応答を検証し、追加する passkey (Candidate) を返す。保存はしない (CommitAdd が、既存の passkey の
// 認可つきで保存する)。すでにある passkey の再登録・UV なしは、error。
func AddFinish(ctx context.Context, cfg Config, st *Store, state, requestID string, resp AttestationResponse) (*Candidate, error) {
	persisted, err := st.load(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := verifyStateToken(persisted.SessionSecret, state, "add-register")
	if err != nil {
		return nil, err
	}
	if claims.RequestID != requestID {
		return nil, errors.New("webauthn: この登録は、別の要求のもの")
	}
	att, err := verifyAttestation(cfg, claims.Challenge, resp, true)
	if err != nil {
		return nil, err
	}
	if persisted.credential(att.cred.ID) != nil {
		return nil, fmt.Errorf("%w: すでに登録済みの passkey", ErrBadOperation)
	}
	if !st.consume("add:"+b64.EncodeToString(claims.Challenge), claims.Expiry) {
		return nil, errors.New("webauthn: この登録は、すでに使われた")
	}
	return &Candidate{
		CredentialID: bytes.Clone(att.cred.ID), Salt: claims.Salt, PRF: att.prf, PRFEnabled: att.prfEnabled,
		cred: att.cred, requestID: requestID, at: time.Now(),
	}, nil
}

// CommitAdd は、認可 (既存の passkey の、OpAddCredential・対象が cand・同じ要求の、OpAuthFinish の結果) と合わせて、cand を
// 保存する。認可が、期限内 (challengeTTL) であること・認可した passkey が、今も登録されていること・数の上限・label (32 文字まで・
// 制御文字なし) を確かめる。Vault のラップ (vault.AddWrap) は、呼び手が、この後に作る (candidate は、web のログインに登録済みで、
// Vault に無い passkey だけ。ADR 0038 決定 3)。
func CommitAdd(ctx context.Context, st *Store, cand *Candidate, authz *Assertion, label string) error {
	if cand == nil || authz == nil {
		return fmt.Errorf("%w: 認可が無い", ErrBadOperation)
	}
	if err := checkLabel(label); err != nil {
		return err
	}
	if authz.op != string(OpAddCredential) || !bytes.Equal(authz.target, cand.CredentialID) || authz.requestID != cand.requestID {
		return fmt.Errorf("%w: 認可が、この追加のものでない", ErrBadOperation)
	}
	if time.Since(authz.at) > challengeTTL || time.Since(cand.at) > 2*challengeTTL {
		return fmt.Errorf("%w: 認可・登録の期限が切れている", ErrBadOperation)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	persisted, err := st.load(ctx)
	if err != nil {
		return err
	}
	if persisted.credential(authz.cred) == nil {
		return fmt.Errorf("%w: 認可した passkey が、もう無い", ErrBadOperation)
	}
	if err := checkOpTarget(persisted, Binding{Op: OpAddCredential, Target: cand.CredentialID}); err != nil {
		return err
	}
	c := cand.cred
	c.Label = []byte(label)
	c.CreatedAt = time.Now().Unix()
	persisted.Credentials = append(persisted.Credentials, c)
	return st.save(persisted)
}

// RemoveCredential は、認可 (削除する passkey とは別の、既存の passkey の、OpRemoveCredential・対象が target・の OpAuthFinish の
// 結果) で、target を削除する。最後の 1 つは消せない。削除は、発行済みのセッションを全て失効させる (世代を進める。失った端末が
// 発行したセッションを、残さない)。Vault のラップの削除 (vault.RemoveWrap) は、呼び手が、この前か後に、別に行う。
func RemoveCredential(ctx context.Context, st *Store, target []byte, authz *Assertion) error {
	if authz == nil {
		return fmt.Errorf("%w: 認可が無い", ErrBadOperation)
	}
	if authz.op != string(OpRemoveCredential) || !bytes.Equal(authz.target, target) {
		return fmt.Errorf("%w: 認可が、この削除のものでない", ErrBadOperation)
	}
	if bytes.Equal(authz.cred, target) {
		return fmt.Errorf("%w: 削除する passkey 自身は、認可に使えない", ErrBadOperation)
	}
	if time.Since(authz.at) > challengeTTL {
		return fmt.Errorf("%w: 認可の期限が切れている", ErrBadOperation)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	persisted, err := st.load(ctx)
	if err != nil {
		return err
	}
	if persisted.credential(authz.cred) == nil {
		return fmt.Errorf("%w: 認可した passkey が、もう無い", ErrBadOperation)
	}
	if err := checkOpTarget(persisted, Binding{Op: OpRemoveCredential, Target: target}); err != nil {
		return err
	}
	kept := persisted.Credentials[:0:0]
	for _, c := range persisted.Credentials {
		if !bytes.Equal(c.ID, target) {
			kept = append(kept, c)
		}
	}
	persisted.Credentials = kept
	persisted.SessionEpoch++
	return st.save(persisted)
}

// CredentialInfo は、登録済みの passkey の公開の情報。
type CredentialInfo struct {
	ID        []byte
	Label     string
	CreatedAt int64
}

// ListCredentials は、登録済みの passkey の一覧を返す。
func ListCredentials(ctx context.Context, st *Store) ([]CredentialInfo, error) {
	state, err := st.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CredentialInfo, 0, len(state.Credentials))
	for _, c := range state.Credentials {
		out = append(out, CredentialInfo{ID: bytes.Clone(c.ID), Label: string(c.Label), CreatedAt: c.CreatedAt})
	}
	return out, nil
}

// checkLabel は、label が、32 文字まで・制御文字なしの UTF-8 か確かめる (空は可)。
func checkLabel(label string) error {
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) > 32 {
		return errors.New("webauthn: label は 32 文字までの UTF-8")
	}
	for _, r := range label {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return errors.New("webauthn: label に制御文字は使えない")
		}
	}
	return nil
}

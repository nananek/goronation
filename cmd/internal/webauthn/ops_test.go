package webauthn_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nananek/goronation/cmd/internal/webauthn"
	"github.com/nananek/goronation/cmd/internal/webauthn/webauthntest"
)

const (
	rpID   = "goro.example.ts.net"
	origin = "https://goro.example.ts.net"
)

var b64 = base64.RawURLEncoding

func cfg() webauthn.Config { return webauthn.Config{RPID: rpID, RPName: "goro test", Origin: origin} }

func newAuth(t *testing.T) *webauthntest.Authenticator {
	t.Helper()
	a, err := webauthntest.New()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// world は、最初の passkey A を登録した状態の Store。
type world struct {
	t  *testing.T
	st *webauthn.Store
	a  *webauthntest.Authenticator
}

func newWorld(t *testing.T) *world {
	t.Helper()
	st, err := webauthn.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tok, err := webauthn.IssueBootstrapToken(ctx, st, 1e9*60)
	if err != nil {
		t.Fatal(err)
	}
	opts, state, err := webauthn.RegisterBegin(ctx, cfg(), st, tok)
	if err != nil {
		t.Fatal(err)
	}
	a := newAuth(t)
	resp, err := a.Register(rpID, origin, opts.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := webauthn.RegisterFinish(ctx, cfg(), st, state, resp); err != nil {
		t.Fatal(err)
	}
	return &world{t: t, st: st, a: a}
}

var ctx = context.Background()

// addPasskey は、既存の passkey (by) の認可で、新しい passkey を追加する (正常系。各段の値を返す)。
func (w *world) addPasskey(by *webauthntest.Authenticator, reqID string) (*webauthntest.Authenticator, *webauthn.Candidate) {
	w.t.Helper()
	b := newAuth(w.t)
	opts, state, err := webauthn.AddBegin(ctx, cfg(), w.st, reqID)
	if err != nil {
		w.t.Fatal(err)
	}
	salt := must(b64.DecodeString(opts.Extensions.PRF.Eval.First))
	att, err := b.RegisterWith(rpID, origin, opts.Challenge, webauthntest.Options{UV: true, Salt: salt})
	if err != nil {
		w.t.Fatal(err)
	}
	cand, err := webauthn.AddFinish(ctx, cfg(), w.st, state, reqID, att)
	if err != nil {
		w.t.Fatal(err)
	}
	authz := w.opAuth(by, webauthn.Binding{Op: webauthn.OpAddCredential, Target: cand.CredentialID, RequestID: reqID}, webauthntest.Options{UV: true})
	if err := webauthn.CommitAdd(ctx, w.st, cand, authz, "phone"); err != nil {
		w.t.Fatal(err)
	}
	return b, cand
}

// opAuth は、by が、b の操作を認可する (PRF の salt は、by が Vault のラップを持つ想定で、固定の salt を使う)。
func (w *world) opAuth(by *webauthntest.Authenticator, b webauthn.Binding, o webauthntest.Options) *webauthn.Assertion {
	w.t.Helper()
	as, err := w.tryOpAuth(by, b, o)
	if err != nil {
		w.t.Fatal(err)
	}
	return as
}

var saltFor = func(id []byte) []byte { return bytes.Repeat([]byte{id[0], 7}, 16) }

func (w *world) tryOpAuth(by *webauthntest.Authenticator, b webauthn.Binding, o webauthntest.Options) (*webauthn.Assertion, error) {
	evals := []webauthn.PRFEval{{CredentialID: by.CredentialID(), Salt: saltFor(by.CredentialID())}}
	opts, state, err := webauthn.OpAuthBegin(ctx, cfg(), w.st, b, evals)
	if err != nil {
		return nil, err
	}
	o.Salt = saltFor(by.CredentialID())
	resp, err := by.AuthenticateWith(rpID, origin, opts.Challenge, o)
	if err != nil {
		w.t.Fatal(err)
	}
	return webauthn.OpAuthFinish(ctx, cfg(), w.st, state, b, resp)
}

func TestAddPasskeyFlow(t *testing.T) {
	w := newWorld(t)
	b, cand := w.addPasskey(w.a, "req-1")
	if string(cand.Salt) == "" || len(cand.Salt) != webauthn.PRFSize {
		t.Fatalf("salt = %x", cand.Salt)
	}
	if !cand.PRFEnabled || !bytes.Equal(cand.PRF, b.PRF(cand.Salt)) {
		t.Fatal("候補の PRF (登録時の results) が、認証器の本物の値と違う")
	}
	list, err := webauthn.ListCredentials(ctx, w.st)
	if err != nil || len(list) != 2 || list[1].Label != "phone" || !bytes.Equal(list[1].ID, b.CredentialID()) {
		t.Fatalf("一覧 = %+v, %v", list, err)
	}
	// 追加した passkey で、ログインできる。元の passkey でも、できる。
	for _, a := range []*webauthntest.Authenticator{b, w.a} {
		opts, state, err := webauthn.AuthenticateBegin(ctx, cfg(), w.st)
		if err != nil || len(opts.AllowCredentials) != 2 {
			t.Fatalf("AuthenticateBegin = %+v, %v", opts, err)
		}
		resp, _ := a.Authenticate(rpID, origin, opts.Challenge)
		if _, err := webauthn.AuthenticateFinish(ctx, cfg(), w.st, state, resp); err != nil {
			t.Fatalf("ログイン: %v", err)
		}
	}
}

func TestOpAuthBeginRequestsPRFPerCredential(t *testing.T) {
	w := newWorld(t)
	b, _ := w.addPasskey(w.a, "r1")
	evals := []webauthn.PRFEval{
		{CredentialID: w.a.CredentialID(), Salt: bytes.Repeat([]byte{1}, 32)},
		{CredentialID: b.CredentialID(), Salt: bytes.Repeat([]byte{2}, 32)},
	}
	opts, _, err := webauthn.OpAuthBegin(ctx, cfg(), w.st, webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "u1"}, evals)
	if err != nil {
		t.Fatal(err)
	}
	if opts.UserVerification != "required" {
		t.Fatalf("userVerification = %q", opts.UserVerification)
	}
	raw, _ := json.Marshal(opts)
	var got struct {
		Extensions struct {
			PRF struct {
				EvalByCredential map[string]struct{ First string } `json:"evalByCredential"`
			} `json:"prf"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	m := got.Extensions.PRF.EvalByCredential
	if len(m) != 2 || m[b64.EncodeToString(w.a.CredentialID())].First != b64.EncodeToString(evals[0].Salt) ||
		m[b64.EncodeToString(b.CredentialID())].First != b64.EncodeToString(evals[1].Salt) {
		t.Fatalf("evalByCredential = %+v (passkey ごとに、別の salt)", m)
	}
	// 未登録の passkey・長さの違う salt・重複は、断る。
	for name, bad := range map[string][]webauthn.PRFEval{
		"unknown": {{CredentialID: []byte("nope"), Salt: evals[0].Salt}},
		"short":   {{CredentialID: w.a.CredentialID(), Salt: []byte("x")}},
		"dup":     {evals[0], evals[0]},
	} {
		if _, _, err := webauthn.OpAuthBegin(ctx, cfg(), w.st, webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "u2"}, bad); err == nil {
			t.Errorf("%s: OpAuthBegin が成功した", name)
		}
	}
}

// クライアントが申告する PRF の値は、署名の外にある: 偽の値は、通る (検証できない)。だから、呼び手は、復号を試す入力にだけ使う。
// ここでは、その境界の挙動 (偽の値が、そのまま届く・本物でない) を固定する。
func TestFakePRFIsPassedThroughAndNotTheRealValue(t *testing.T) {
	w := newWorld(t)
	fake := bytes.Repeat([]byte{0xee}, 32)
	as, err := w.tryOpAuth(w.a, webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "u1"}, webauthntest.Options{UV: true, FakePRF: fake})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(as.PRF, fake) || bytes.Equal(as.PRF, w.a.PRF(saltFor(w.a.CredentialID()))) {
		t.Fatal("偽の PRF の値の扱いが、想定と違う")
	}
	as.Wipe()
	if as.PRF != nil {
		t.Fatal("Wipe が PRF を消していない")
	}
}

func TestOpAuthFinishRejections(t *testing.T) {
	w := newWorld(t)
	good := webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "u1"}
	for name, tc := range map[string]struct {
		opt webauthntest.Options
		// Finish に渡す束縛 (Begin と違うもの)
		finish *webauthn.Binding
	}{
		"no UV":             {opt: webauthntest.Options{UV: false}},
		"wrong-length PRF":  {opt: webauthntest.Options{UV: true, FakePRF: []byte("short")}},
		"ext data ok→bad":   {opt: webauthntest.Options{UV: true, RawExtData: []byte{0xa1, 0x63, 'f', 'o', 'o', 0x01}}},
		"different request": {opt: webauthntest.Options{UV: true}, finish: &webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "other"}},
		"different op":      {opt: webauthntest.Options{UV: true}, finish: &webauthn.Binding{Op: webauthn.OpAddCredential, Target: []byte("x"), RequestID: "u1"}},
	} {
		evals := []webauthn.PRFEval{{CredentialID: w.a.CredentialID(), Salt: saltFor(w.a.CredentialID())}}
		opts, state, err := webauthn.OpAuthBegin(ctx, cfg(), w.st, good, evals)
		if err != nil {
			t.Fatal(err)
		}
		o := tc.opt
		if o.FakePRF == nil {
			o.Salt = saltFor(w.a.CredentialID())
		}
		resp, _ := w.a.AuthenticateWith(rpID, origin, opts.Challenge, o)
		b := good
		if tc.finish != nil {
			b = *tc.finish
		}
		if as, err := webauthn.OpAuthFinish(ctx, cfg(), w.st, state, b, resp); err == nil {
			t.Errorf("%s: OpAuthFinish が成功した (%+v)", name, as)
		}
	}
	// PRF を要求したのに、結果が無い。
	evals := []webauthn.PRFEval{{CredentialID: w.a.CredentialID(), Salt: saltFor(w.a.CredentialID())}}
	opts, state, _ := webauthn.OpAuthBegin(ctx, cfg(), w.st, good, evals)
	resp, _ := w.a.AuthenticateWith(rpID, origin, opts.Challenge, webauthntest.Options{UV: true})
	if _, err := webauthn.OpAuthFinish(ctx, cfg(), w.st, state, good, resp); err == nil {
		t.Error("PRF の結果が無いのに成功した")
	}
}

func TestOpAuthReplayIsRejected(t *testing.T) {
	w := newWorld(t)
	b := webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "u1"}
	evals := []webauthn.PRFEval{{CredentialID: w.a.CredentialID(), Salt: saltFor(w.a.CredentialID())}}
	opts, state, _ := webauthn.OpAuthBegin(ctx, cfg(), w.st, b, evals)
	resp, _ := w.a.AuthenticateWith(rpID, origin, opts.Challenge, webauthntest.Options{UV: true, Salt: saltFor(w.a.CredentialID())})
	if _, err := webauthn.OpAuthFinish(ctx, cfg(), w.st, state, b, resp); err != nil {
		t.Fatal(err)
	}
	if _, err := webauthn.OpAuthFinish(ctx, cfg(), w.st, state, b, resp); err == nil {
		t.Fatal("同じ応答の再送が通った")
	}
}

func TestStateTokensAreNotInterchangeable(t *testing.T) {
	w := newWorld(t)
	b := webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "u1"}
	evals := []webauthn.PRFEval{{CredentialID: w.a.CredentialID(), Salt: saltFor(w.a.CredentialID())}}
	opts, opState, _ := webauthn.OpAuthBegin(ctx, cfg(), w.st, b, evals)
	resp, _ := w.a.AuthenticateWith(rpID, origin, opts.Challenge, webauthntest.Options{UV: true, Salt: saltFor(w.a.CredentialID())})
	// 操作つきの認証の state で、ログイン (セッション token の発行) はできない。
	if tok, err := webauthn.AuthenticateFinish(ctx, cfg(), w.st, opState, resp); err == nil {
		t.Fatalf("op-auth の state で、セッションを得られた: %q", tok)
	}
	// ログインの state で、操作の認可は得られない。
	lopts, lstate, _ := webauthn.AuthenticateBegin(ctx, cfg(), w.st)
	lresp, _ := w.a.AuthenticateWith(rpID, origin, lopts.Challenge, webauthntest.Options{UV: true, Salt: saltFor(w.a.CredentialID())})
	if _, err := webauthn.OpAuthFinish(ctx, cfg(), w.st, lstate, b, lresp); err == nil {
		t.Fatal("ログインの state で、操作の認可を得られた")
	}
	// 追加の登録の state も、別の目的には使えない。
	_, addState, _ := webauthn.AddBegin(ctx, cfg(), w.st, "r1")
	if _, err := webauthn.OpAuthFinish(ctx, cfg(), w.st, addState, b, lresp); err == nil {
		t.Fatal("add-register の state が、op-auth に通った")
	}
}

func TestAddRejections(t *testing.T) {
	w := newWorld(t)
	// すでにある passkey の再登録 (excludeCredentials を無視する認証器)。
	opts, state, err := webauthn.AddBegin(ctx, cfg(), w.st, "r1")
	if err != nil || len(opts.ExcludeCredentials) != 1 || opts.AuthenticatorSelection.UserVerification != "required" {
		t.Fatalf("AddBegin = %+v, %v", opts, err)
	}
	re, _ := w.a.RegisterWith(rpID, origin, opts.Challenge, webauthntest.Options{UV: true})
	if _, err := webauthn.AddFinish(ctx, cfg(), w.st, state, "r1", re); err == nil {
		t.Error("登録済みの passkey の再登録が通った")
	}
	// UV なし・別の要求の ID・再送。
	b := newAuth(t)
	opts, state, _ = webauthn.AddBegin(ctx, cfg(), w.st, "r2")
	noUV, _ := b.RegisterWith(rpID, origin, opts.Challenge, webauthntest.Options{})
	if _, err := webauthn.AddFinish(ctx, cfg(), w.st, state, "r2", noUV); err == nil {
		t.Error("UV なしの登録が通った")
	}
	ok, _ := b.RegisterWith(rpID, origin, opts.Challenge, webauthntest.Options{UV: true})
	if _, err := webauthn.AddFinish(ctx, cfg(), w.st, state, "other", ok); err == nil {
		t.Error("別の要求の ID で、登録が通った")
	}
	if _, err := webauthn.AddFinish(ctx, cfg(), w.st, state, "r2", ok); err != nil {
		t.Fatal(err)
	}
	if _, err := webauthn.AddFinish(ctx, cfg(), w.st, state, "r2", ok); err == nil {
		t.Error("同じ登録の再送が通った")
	}
	// 未登録の Store では、追加を始められない。
	empty, _ := webauthn.NewStore(t.TempDir())
	if _, _, err := webauthn.AddBegin(ctx, cfg(), empty, "r3"); err == nil {
		t.Error("未登録の Store で AddBegin が成功した")
	}
	// 形の不正な要求の ID。
	for _, id := range []string{"", "a b", strings.Repeat("a", 65), "a\n"} {
		if _, _, err := webauthn.AddBegin(ctx, cfg(), w.st, id); err == nil {
			t.Errorf("要求の ID %q が通った", id)
		}
	}
}

func TestCommitAddRequiresMatchingAuthorization(t *testing.T) {
	w := newWorld(t)
	b := newAuth(t)
	opts, state, _ := webauthn.AddBegin(ctx, cfg(), w.st, "r1")
	att, _ := b.RegisterWith(rpID, origin, opts.Challenge, webauthntest.Options{UV: true})
	cand, err := webauthn.AddFinish(ctx, cfg(), w.st, state, "r1", att)
	if err != nil {
		t.Fatal(err)
	}
	wrong := map[string]*webauthn.Assertion{
		"nil":           nil,
		"unlock":        w.opAuth(w.a, webauthn.Binding{Op: webauthn.OpUnlock, RequestID: "r1"}, webauthntest.Options{UV: true}),
		"other target":  w.opAuth(w.a, webauthn.Binding{Op: webauthn.OpAddCredential, Target: []byte("other-credential"), RequestID: "r1"}, webauthntest.Options{UV: true}),
		"other request": w.opAuth(w.a, webauthn.Binding{Op: webauthn.OpAddCredential, Target: cand.CredentialID, RequestID: "r9"}, webauthntest.Options{UV: true}),
	}
	for name, authz := range wrong {
		if err := webauthn.CommitAdd(ctx, w.st, cand, authz, ""); err == nil {
			t.Errorf("%s: CommitAdd が成功した", name)
		}
	}
	if list, _ := webauthn.ListCredentials(ctx, w.st); len(list) != 1 {
		t.Fatalf("認可なしで、passkey が増えた: %d", len(list))
	}
	// 候補自身 (まだ未登録) は、自分の追加を認可できない。
	if _, err := w.tryOpAuth(b, webauthn.Binding{Op: webauthn.OpAddCredential, Target: cand.CredentialID, RequestID: "r1"}, webauthntest.Options{UV: true}); err == nil {
		t.Error("未登録の候補が、自分の追加を認可できた")
	}
	good := w.opAuth(w.a, webauthn.Binding{Op: webauthn.OpAddCredential, Target: cand.CredentialID, RequestID: "r1"}, webauthntest.Options{UV: true})
	if err := webauthn.CommitAdd(ctx, w.st, cand, good, "bad\nlabel"); err == nil {
		t.Error("制御文字のある label が通った")
	}
	if err := webauthn.CommitAdd(ctx, w.st, cand, good, "電話 (仕事用)"); err != nil {
		t.Fatal(err)
	}
	if list, err := webauthn.ListCredentials(ctx, w.st); err != nil || len(list) != 2 || list[1].Label != "電話 (仕事用)" {
		t.Fatalf("空白・非 ASCII の label が、保存・復元できない: %+v, %v", list, err)
	}
	if err := webauthn.CommitAdd(ctx, w.st, cand, good, ""); err == nil {
		t.Error("同じ追加を、2 回 Commit できた")
	}
}

func TestRemoveCredential(t *testing.T) {
	w := newWorld(t)
	b, _ := w.addPasskey(w.a, "r1")
	remB := webauthn.Binding{Op: webauthn.OpRemoveCredential, Target: b.CredentialID(), RequestID: "d1"}

	// 消す passkey 自身は、認可に使えない (Begin の allowCredentials から外れ、Finish も断る)。
	if _, err := w.tryOpAuth(b, remB, webauthntest.Options{UV: true}); err == nil {
		t.Fatal("消す passkey 自身が、削除を認可できた")
	}
	sess := func() string {
		o, s, _ := webauthn.AuthenticateBegin(ctx, cfg(), w.st)
		r, _ := w.a.Authenticate(rpID, origin, o.Challenge)
		tok, err := webauthn.AuthenticateFinish(ctx, cfg(), w.st, s, r)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}()
	// 別の対象の認可では、消せない。
	c, _ := w.addPasskey(w.a, "r2")
	other := w.opAuth(w.a, webauthn.Binding{Op: webauthn.OpRemoveCredential, Target: c.CredentialID(), RequestID: "d2"}, webauthntest.Options{UV: true})
	if err := webauthn.RemoveCredential(ctx, w.st, b.CredentialID(), other); err == nil {
		t.Error("別の対象の認可で、削除できた")
	}
	authz := w.opAuth(w.a, remB, webauthntest.Options{UV: true})
	if err := webauthn.RemoveCredential(ctx, w.st, b.CredentialID(), nil); err == nil {
		t.Error("認可なしで削除できた")
	}
	if err := webauthn.RemoveCredential(ctx, w.st, b.CredentialID(), authz); err != nil {
		t.Fatal(err)
	}
	if err := webauthn.RemoveCredential(ctx, w.st, c.CredentialID(), w.opAuth(w.a, webauthn.Binding{Op: webauthn.OpRemoveCredential, Target: c.CredentialID(), RequestID: "d4"}, webauthntest.Options{UV: true})); err != nil {
		t.Fatal(err)
	}
	if list, _ := webauthn.ListCredentials(ctx, w.st); len(list) != 1 || !bytes.Equal(list[0].ID, w.a.CredentialID()) {
		t.Fatalf("一覧 = %+v", list)
	}
	if err := webauthn.VerifySession(ctx, w.st, sess); err == nil {
		t.Error("削除の後も、発行済みのセッションが有効")
	}
	// 削除した passkey は、ログインできない。
	o, s, _ := webauthn.AuthenticateBegin(ctx, cfg(), w.st)
	r, _ := b.Authenticate(rpID, origin, o.Challenge)
	if _, err := webauthn.AuthenticateFinish(ctx, cfg(), w.st, s, r); err == nil {
		t.Error("削除した passkey で、ログインできた")
	}
	// 最後の 1 つは、消せない (Begin の段階で断る)。
	if _, _, err := webauthn.OpAuthBegin(ctx, cfg(), w.st, webauthn.Binding{Op: webauthn.OpRemoveCredential, Target: w.a.CredentialID(), RequestID: "d3"}, nil); err == nil {
		t.Error("最後の 1 つの削除を始められた")
	}
}

func TestCredentialLimit(t *testing.T) {
	w := newWorld(t)
	for i := 1; i < 16; i++ {
		w.addPasskey(w.a, "r"+strings.Repeat("x", i))
	}
	if _, _, err := webauthn.AddBegin(ctx, cfg(), w.st, "over"); err == nil {
		t.Fatal("16 個を超える追加を、始められた")
	}
}

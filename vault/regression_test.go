package vault

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
)

// fakePRFValues は、「クライアントが申告する、偽の PRF 値」の集まり。PRF の結果は署名の外にあり、クライアントが任意の値を送れる
// (ADR 0033 決定 5)。
func fakePRFValues(t *testing.T) map[string][]byte {
	t.Helper()
	random := make([]byte, PRFSize)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(fakePRF("a"))
	flipped[31] ^= 1
	return map[string][]byte{
		"zero":            make([]byte, PRFSize),
		"ff":              bytes.Repeat([]byte{0xff}, PRFSize),
		"random":          random,
		"one bit flipped": flipped,
		"other passkey":   fakePRF("b"),
		"empty":           {},
		"nil":             nil,
		"short":           fakePRF("a")[:31],
		"long":            append(bytes.Clone(fakePRF("a")), 0),
	}
}

// TestFakePRFNeverChangesWraps は、偽の PRF 値を、全ての入口 (Unlock・AddWrap・RemoveWrap・Init) に送っても、ラップ (vault.json 全体) が
// 変わらないことを固定する (ccserver の PR 190 の教訓・ADR 0033 決定 5)。
func TestFakePRFNeverChangesWraps(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-b", "b")
	v.Lock()
	before := snapshot(t, dir)
	infos := v.WrapInfos()
	salt := fakeSalt(t)

	for name, fake := range fakePRFValues(t) {
		for _, id := range []string{"cred-a", "cred-b", "cred-unknown"} {
			if id == "cred-b" && name == "other passkey" {
				continue // これは、cred-b の本物の PRF
			}
			label := fmt.Sprintf("%s/%s", name, id)
			if err := v.Unlock(id, fake); err == nil {
				t.Errorf("%s: 偽の PRF で Unlock できた", label)
			}
			if v.Unlocked() {
				t.Fatalf("%s: 偽の PRF で解錠された", label)
			}
			if err := v.AddWrap(Proof{CredentialID: id, PRF: fake}, Enrollment{CredentialID: "cred-c", Salt: salt, PRF: fakePRF("c")}); err == nil {
				t.Errorf("%s: 偽の PRF を authorizer にして、追加できた", label)
			}
			// 追加候補の値として、偽の PRF を送ることは、ラップを作るだけ (検証できない値) だが、既存のラップは、上書きしない。
			auth := Proof{CredentialID: "cred-a", PRF: fakePRF("a")}
			for _, existing := range []string{"cred-a", "cred-b"} {
				err := v.AddWrap(auth, Enrollment{CredentialID: existing, Salt: salt, PRF: fake})
				if !errors.Is(err, ErrWrapExists) && !errors.Is(err, ErrInvalidInput) {
					t.Errorf("%s: 既存の %s への追加 = %v, want ErrWrapExists か ErrInvalidInput", label, existing, err)
				}
			}
		}
		mustEqual(t, "vault.json ("+name+")", snapshot(t, dir), before)
	}

	// 解錠中の RemoveWrap も、偽の PRF では、何も消えない。
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatal(err)
	}
	for name, fake := range fakePRFValues(t) {
		if name == "other passkey" {
			continue
		}
		if err := v.RemoveWrap(Proof{CredentialID: "cred-a", PRF: fake}, "cred-b"); err == nil {
			t.Errorf("%s: 偽の PRF で削除できた", name)
		}
		if err := v.RemoveWrap(Proof{CredentialID: "cred-b", PRF: fake}, "cred-a"); err == nil {
			t.Errorf("%s: 偽の PRF で削除できた (逆向き)", name)
		}
	}
	if err := v.Init("cred-z", salt, fakePRF("z")); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("初期化済みの Init = %v", err)
	}
	mustEqual(t, "vault.json (最後)", snapshot(t, dir), before)
	if got := v.WrapInfos(); len(got) != len(infos) {
		t.Fatalf("ラップの数が変わった: %d → %d", len(infos), len(got))
	}
	// 持ち主は、締め出されていない。
	v.Lock()
	for id, seed := range map[string]string{"cred-a": "a", "cred-b": "b"} {
		if err := v.Unlock(id, fakePRF(seed)); err != nil {
			t.Errorf("%s が、Unlock できなくなった: %v", id, err)
		}
	}
}

// TestOverwriteAttemptCannotLockOutOwner は、F2 (ccserver の過去の脆弱性) の再現の試み: 追加の口を持つ者が、正当な passkey のラップを、
// 自分の値で上書きして、持ち主を締め出す。追加だけの API では、できない。
func TestOverwriteAttemptCannotLockOutOwner(t *testing.T) {
	v, dir := initVault(t)
	addPasskey(t, v, "cred-victim", "victim")
	before := snapshot(t, dir)
	attacker := Enrollment{CredentialID: "cred-victim", Salt: fakeSalt(t), PRF: fakePRF("attacker")}
	// 認可 (authorizer の証明) が本物でも、既存のラップは置き換わらない。
	if err := v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, attacker); !errors.Is(err, ErrWrapExists) {
		t.Fatalf("上書きの試み = %v, want ErrWrapExists", err)
	}
	mustEqual(t, "vault.json", snapshot(t, dir), before)
	v.Lock()
	if err := v.Unlock("cred-victim", fakePRF("victim")); err != nil {
		t.Fatalf("持ち主が Unlock できなくなった: %v", err)
	}
}

// TestBogusCandidateWrapIsHarmless は、検証できない candidate の PRF (偽の値) で作ったラップが、他のラップに影響しないことを確かめる。
// 認可は authorizer だけで行う (ADR 0038)。偽の値のラップは、誰にも開けない、ただのゴミになる。
func TestBogusCandidateWrapIsHarmless(t *testing.T) {
	v, _ := initVault(t)
	if err := v.AddWrap(Proof{CredentialID: "cred-a", PRF: fakePRF("a")}, Enrollment{CredentialID: "cred-bogus", Salt: fakeSalt(t), PRF: fakePRF("claimed")}); err != nil {
		t.Fatal(err)
	}
	v.Lock()
	if err := v.Unlock("cred-bogus", fakePRF("the-real-one")); !errors.Is(err, ErrUnlockFailed) {
		t.Fatalf("本物の PRF が、申告した値と違えば Unlock できない: %v", err)
	}
	if err := v.Unlock("cred-a", fakePRF("a")); err != nil {
		t.Fatalf("他の passkey は影響を受けない: %v", err)
	}
}

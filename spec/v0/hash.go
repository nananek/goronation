package v0

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

// 内容ハッシュ (ADR 0042): 利用者に見せた要求の内容を、1 つの値にして、応答で写させる。応答の ContentHash が、保持した要求の
// ContentHash と違えば、拒否する。これが守るのは、「利用者が見た内容」と「goronation が保持した内容」が、食い違ったまま承認される
// 事故 (古い画面・別の要求の取り違え・通知からの承認) で、セッションを乗っ取った者は守らない (ハッシュは、同じ SSE で読める)。

// HashPrefix は、ハッシュの値の接頭辞 (アルゴリズムの名前)。
const HashPrefix = "sha256:"

// Hash は、要求の内容の正規形 (キーを辞書順にした JSON。ContentHash 以外の全ての欄) の SHA-256 を、"sha256:<16 進>" で返す。
// 同じ内容は、いつも同じ値になる。input は、書き換えずに (数は、元の文字列のまま) 含める。
func (p PermissionRequested) Hash() string {
	q := p
	q.ContentHash = ""
	return hashOf("permission.requested", q)
}

// Hash は、form の要求の内容の SHA-256 (PermissionRequested.Hash と同じ規則)。
func (f FormRequested) Hash() string {
	g := f
	g.ContentHash = ""
	return hashOf("form.requested", g)
}

func hashOf(typ string, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "" // 到達しない (欄は、全て JSON にできる型)。空は、どの応答とも一致しない
	}
	canon, err := canonicalJSON(b)
	if err != nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("goronation/v0/" + typ + "\n"))
	h.Write(canon)
	return HashPrefix + hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON は、JSON を、キーを辞書順にして、空白と HTML のエスケープなしで、書き直す。数は、元の文字列のまま (json.Number)。
func canonicalJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("v0: JSON の後に余りがある")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // map のキーは、辞書順で書かれる
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// ErrHashMismatch は、応答の ContentHash が、保持した要求の値と違うときの error。
var ErrHashMismatch = errors.New("v0: content hash mismatch")

// ErrHashRequired は、ハッシュが必須なのに、応答に無いときの error。
var ErrHashRequired = errors.New("v0: content hash required")

// VerifyHash は、応答の ContentHash (got) が、保持した要求の値 (want) と一致することを確かめる (時間が一定の比較)。
// require が false のときだけ、got が空でもよい (導入の途中の互換。M2 の本実装は require=true)。want が空 (保持していない) のときは、
// 何も言えないので、require に依らず、got が空でなければ不一致にする。
func VerifyHash(want, got string, require bool) error {
	if got == "" {
		if require {
			return ErrHashRequired
		}
		return nil
	}
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return ErrHashMismatch
	}
	return nil
}

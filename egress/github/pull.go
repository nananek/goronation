package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/nananek/goronation/egress/git"
)

// 要求の大きさの上限。JSON の文字列は、制御文字を \u00XX (6 バイト) に書けるので、本文 64 KiB の全体の上限は、それより大きい。
const (
	MaxRequestBytes = 512 << 10
	MaxTitleBytes   = 256
	MaxBodyBytes    = 64 << 10
)

// allowedFields は、要求に書いてよい項目 (完全一致。大文字違いは別の項目として断る)。
var allowedFields = map[string]bool{"title": true, "body": true, "head": true, "base": true, "draft": true}

// PullPolicy は、檻が作ってよい PR の範囲。
type PullPolicy struct {
	// Push は、head を置く repo (push 先) と、セッション。head は、この Policy の名前空間のブランチだけ。
	Push git.Policy
	// Targets は、PR を作ってよい repo (base の側)。空なら、Push.Repo だけ。Push.Repo と違う repo は、Push.Repo の fork を指す想定 (head は owner:branch の形)。
	Targets []git.Repo
}

// Pull は、検査を通した PR の作成要求。
type Pull struct {
	// Repo は、PR を作る repo (許可した綴り)。
	Repo git.Repo
	// Title・Body は、PR の題と本文。Head は、上流に送る head (同じ repo なら branch 名、fork なら owner:branch)。Base は、空なら、呼び手が WithBase で埋める。
	Title, Body, Head, Base string
}

// ParsePull は、repo に PR を作る要求の本文 body を検査し、Pull にする。
// repo が Targets に無ければ CodeRepoNotAllowed、head が許可した名前空間のブランチでなければ CodeHeadNotAllowed。それ以外は、CodeTooLarge・CodeBadJSON・CodeUnknownField・CodeDuplicateField・CodeBadField。
// 項目は title (必須)・body・head (必須)・base・draft だけ。draft は、bool ならよいが、値に依らず、Pull.JSON が送るのは常に true (draft: false は上書きされる)。
func (pp PullPolicy) ParsePull(repo git.Repo, body []byte) (Pull, error) {
	target, ok := pp.target(repo)
	if !ok {
		return Pull{}, reject(CodeRepoNotAllowed, "repo %s には PR を作れない", q(repo.String()))
	}
	if len(body) > MaxRequestBytes {
		return Pull{}, reject(CodeTooLarge, "要求が %d バイトを超える", MaxRequestBytes)
	}
	if !utf8.Valid(body) {
		return Pull{}, reject(CodeBadJSON, "UTF-8 として正しくない")
	}
	fields, err := decodeFields(body)
	if err != nil {
		return Pull{}, err
	}
	var p Pull
	p.Repo = target
	for _, f := range []struct {
		name     string
		dst      *string
		required bool
	}{{"title", &p.Title, true}, {"body", &p.Body, false}, {"head", &p.Head, true}, {"base", &p.Base, false}} {
		raw, present := fields[f.name]
		if !present {
			if f.required {
				return Pull{}, reject(CodeBadField, "%s が無い", f.name)
			}
			continue
		}
		if *f.dst, err = decodeString(f.name, raw); err != nil {
			return Pull{}, err
		}
	}
	if raw, present := fields["draft"]; present && string(raw) != "true" && string(raw) != "false" {
		return Pull{}, reject(CodeBadField, "draft が bool ではない")
	}
	if err := checkText("title", p.Title, MaxTitleBytes, false); err != nil {
		return Pull{}, err
	}
	if strings.TrimSpace(p.Title) == "" {
		return Pull{}, reject(CodeBadField, "title が空白だけ")
	}
	if err := checkText("body", p.Body, MaxBodyBytes, true); err != nil {
		return Pull{}, err
	}
	if p.Head, err = pp.head(target, p.Head); err != nil {
		return Pull{}, err
	}
	if _, present := fields["base"]; present {
		if err := git.CheckBranchName(p.Base); err != nil { // 書いたなら、空文字も断る (省くときは、項目ごと省く)
			return Pull{}, reject(CodeBadField, "base が branch 名として正しくない: %s", q(p.Base))
		}
	}
	return p, nil
}

// target は、repo が PR を作ってよい repo なら、許可した綴りの repo を返す。
func (pp PullPolicy) target(repo git.Repo) (git.Repo, bool) {
	targets := pp.Targets
	if len(targets) == 0 {
		targets = []git.Repo{pp.Push.Repo}
	}
	for _, t := range targets {
		if t.Equal(repo) {
			return t, true
		}
	}
	return git.Repo{}, false
}

// head は、head の値 ("branch" か "owner:branch") を検査し、上流に送る形にする。
// branch は、Push の名前空間の中。owner は、Push.Repo の owner だけ。PR を作る repo が Push.Repo なら、送るのは branch だけ。
// Push.Repo と違う repo (fork の元) なら、owner:branch の形でなければ断る (branch が、PR を作る repo の側のものと取り違えられないため)。
func (pp PullPolicy) head(target git.Repo, head string) (string, error) {
	owner, branch, hasOwner := strings.Cut(head, ":")
	if !hasOwner {
		owner, branch = "", head
	}
	if err := pp.Push.CheckBranch(branch); err != nil {
		return "", reject(CodeHeadNotAllowed, "head %s は、許可したブランチ (%s の下) ではない", q(head), pp.Push.Prefix()[len("refs/heads/"):])
	}
	same := target.Equal(pp.Push.Repo)
	switch {
	case hasOwner && !strings.EqualFold(owner, pp.Push.Repo.Owner):
		return "", reject(CodeHeadNotAllowed, "head %s の owner が、許可した owner ではない", q(head))
	case !hasOwner && !same:
		return "", reject(CodeHeadNotAllowed, "repo %s への PR の head は、owner:branch の形で書く", q(target.String()))
	case same:
		return branch, nil
	}
	return pp.Push.Repo.Owner + ":" + branch, nil
}

// decodeFields は、body を、項目 → 値 (JSON のまま) に分ける。項目は allowedFields の完全一致だけで、重複・未知・末尾の続きは断る。
// 未知の項目は、値を読む前に断る (深い入れ子を読まない)。
func decodeFields(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, reject(CodeBadJSON, "JSON のオブジェクトではない")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		key, isString := tok.(string)
		if err != nil || !isString {
			return nil, reject(CodeBadJSON, "項目の名前を読めない")
		}
		switch {
		case !allowedFields[key]:
			return nil, reject(CodeUnknownField, "項目 %s は許可していない", q(key))
		case fields[key] != nil:
			return nil, reject(CodeDuplicateField, "項目 %s が 2 回ある", q(key))
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, reject(CodeBadJSON, "項目 %s の値を読めない", q(key))
		}
		fields[key] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, reject(CodeBadJSON, "オブジェクトが閉じていない")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, reject(CodeBadJSON, "オブジェクトの後に、続きがある")
	}
	return fields, nil
}

// decodeString は、JSON の文字列 (null・数・配列などは断る) を Go の文字列にする。
func decodeString(field string, raw json.RawMessage) (string, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", reject(CodeBadField, "%s が文字列ではない", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", reject(CodeBadJSON, "%s の文字列を読めない", field)
	}
	return s, nil
}

// checkText は、題・本文の文字列を検査する: 大きさ (バイト) と、forbiddenRune の文字 (制御文字・書式制御・見えない文字など) が無いこと。
func checkText(field, s string, max int, multiline bool) error {
	if len(s) > max {
		return reject(CodeBadField, "%s が %d バイトを超える", field, max)
	}
	var prev rune
	for _, r := range s {
		if forbiddenRune(prev, r, multiline) {
			return reject(CodeBadField, "%s に、使えない文字 U+%04X (制御文字・書式制御・見えない文字など) がある", field, r)
		}
		prev = r
	}
	return nil
}

// WithBase は、base を name にした Pull を返す (要求が base を省いたとき、上流の既定の branch で埋める)。name は、branch 名として正しくなければ断る。
func (p Pull) WithBase(name string) (Pull, error) {
	if err := git.CheckBranchName(name); err != nil {
		return Pull{}, reject(CodeBadField, "base %s が branch 名として正しくない", q(name))
	}
	p.Base = name
	return p, nil
}

// wirePull は、上流に送る JSON。項目の並びと名前が固定で、draft は常に true。
type wirePull struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Draft bool   `json:"draft"`
}

// JSON は、上流 (POST /repos/<o>/<r>/pulls) に送る本文を返す。受けた JSON でなく、検査した値から作り直し、draft は常に true。
// Head か Base が空 (base を WithBase で埋めていない) なら、error。
func (p Pull) JSON() ([]byte, error) {
	if p.Head == "" || p.Base == "" {
		return nil, errors.New("github: head と base を埋めてから JSON にする")
	}
	return json.Marshal(wirePull{Title: p.Title, Body: p.Body, Head: p.Head, Base: p.Base, Draft: true})
}

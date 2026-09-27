package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nananek/goronation/egress/git"
)

// MaxResponseBytes は、上流の応答として読む大きさの上限 (PR の作成・repo の情報の応答)。
const MaxResponseBytes = 4 << 20

// Result は、檻に返す、PR の作成の結果。上流の応答のうち、この 2 つだけ (他の項目・ヘッダは返さない)。
type Result struct {
	Number int
	// URL は、https://github.com/<owner>/<name>/pull/<Number> の形 (owner・name は、要求した repo の綴り)。
	URL string
	// Draft は、上流が、draft と答えたか。false なら、draft の強制が効かなかった (呼び手が、監査に残す)。
	Draft bool
}

// ParseResult は、PR の作成の応答の本文を検査し、Result にする。number が 1 以上で、html_url が repo の PR の URL (number と一致) でなければ、CodeBadResponse。
// 返す URL は、応答の文字列ではなく、検査した値から作り直した文字列。
func ParseResult(body []byte, repo git.Repo) (Result, error) {
	if len(body) > MaxResponseBytes {
		return Result{}, reject(CodeTooLarge, "応答が %d バイトを超える", MaxResponseBytes)
	}
	var v struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Draft   *bool  `json:"draft"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return Result{}, reject(CodeBadResponse, "応答を JSON として読めない")
	}
	want := fmt.Sprintf("https://github.com/%s/%s/pull/%d", repo.Owner, repo.Name, v.Number)
	if v.Number < 1 || !strings.EqualFold(v.HTMLURL, want) {
		return Result{}, reject(CodeBadResponse, "number と html_url が、repo %s の PR として正しくない", q(repo.String()))
	}
	return Result{Number: v.Number, URL: want, Draft: v.Draft != nil && *v.Draft}, nil
}

// JSON は、檻に返す本文 {"number":N,"html_url":"…"} を返す。
func (r Result) JSON() []byte {
	b, _ := json.Marshal(struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}{r.Number, r.URL})
	return b
}

// DefaultBranch は、repo の情報の応答の本文から、既定の branch 名を取り出す。branch 名として正しくなければ、CodeBadResponse。
func DefaultBranch(body []byte) (string, error) {
	if len(body) > MaxResponseBytes {
		return "", reject(CodeTooLarge, "応答が %d バイトを超える", MaxResponseBytes)
	}
	var v struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", reject(CodeBadResponse, "応答を JSON として読めない")
	}
	if err := git.CheckBranchName(v.DefaultBranch); err != nil {
		return "", reject(CodeBadResponse, "default_branch %s が branch 名として正しくない", q(v.DefaultBranch))
	}
	return v.DefaultBranch, nil
}

// Quota は、1 セッションが PR を作る試行の回数の枠。並行して使える。
type Quota struct {
	mu   sync.Mutex
	left int
}

// NewQuota は、n 回の枠を作る (n が 0 以下なら、1 回も許さない)。
func NewQuota(n int) *Quota { return &Quota{left: max(n, 0)} }

// Take は、枠を 1 回分使う。残りが無ければ、CodeQuota。上流が断った試行も、数える (失敗を重ねて、上流を叩き続けさせない)。
func (qt *Quota) Take() error {
	qt.mu.Lock()
	defer qt.mu.Unlock()
	if qt.left == 0 {
		return reject(CodeQuota, "このセッションの PR の作成の枠を使い切った")
	}
	qt.left--
	return nil
}

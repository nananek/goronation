package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/nananek/goronation/core/credential"
	"github.com/nananek/goronation/egress/git"
	"github.com/nananek/goronation/egress/github"
)

// CredentialName は、Config.Credentials から引く、GitHub のトークンの名前。
const CredentialName = "github"

// 既定値。Config の 0 (未設定) は、これになる。
const (
	DefaultMaxPackBytes = 512 << 20
	DefaultPRQuota      = 5
	defaultGitBaseURL   = "https://github.com"
	defaultAPIBaseURL   = "https://api.github.com"
	// maxResponseBytes は、PR 作成・repo 情報・receive-pack の応答として読む大きさの上限 (どれも小さい JSON か report-status)。
	// fetch・push の本文 (pack) 自体の上限は、Config.MaxPackBytes (別)。
	maxResponseBytes = 4 << 20
)

// Config は、Handler の設定。1 つの Handler は、1 つの Push (repo とセッション) だけを扱う (goro run 1 回に対応)。
type Config struct {
	// Push は、檻から push・fetch を許す、唯一の repo とセッション。git.NewPolicy で作ったものだけを受け付ける
	// (構造体リテラルで作った、検査を経ていない Policy は、New が断る。攻撃者視点レビュー L2)。
	Push git.Policy
	// Pull は、PR を作ってよい repo の policy (Push を含み、Targets が空なら Push.Repo だけ)。Push と同じく、値を検査する。
	Pull github.PullPolicy
	// Credentials は、上流へ渡す資格情報の取り出し口 (CredentialName で引く)。必須。
	Credentials credential.Source
	// GitBaseURL・APIBaseURL は、上流の scheme://host[:port] (末尾に / を付けない)。空なら、それぞれ github.com・api.github.com。
	// テストは、ここに偽の上流 (httptest.Server.URL) を指定する。
	GitBaseURL, APIBaseURL string
	// MaxPackBytes は、receive-pack の本文 (コマンド部 + pack) と、fetch の応答の上限。0 以下なら DefaultMaxPackBytes。
	MaxPackBytes int64
	// PRQuota は、1 Handler (1 セッション) が作れる PR の数。0 以下なら DefaultPRQuota。
	PRQuota int
	// Audit は、監査 (JSON 1 行ずつ) の出力先。nil なら io.Discard。
	Audit io.Writer
}

// Handler は、gateway の http.Handler。New で作る。
type Handler struct {
	push    git.Policy
	pull    github.PullPolicy
	creds   credential.Source
	gitBase string
	apiBase string
	maxPack int64
	quota   *github.Quota
	audit   io.Writer
	auditMu sync.Mutex
	client  *http.Client
}

var _ http.Handler = (*Handler)(nil)

// New は、cfg から Handler を作る。Push・Pull.Push が git.NewPolicy を経た値でない、Credentials が nil、base URL の形が
// 正しくない、のいずれかなら error (何も中継しない)。
func New(cfg Config) (*Handler, error) {
	if err := checkPolicy(cfg.Push); err != nil {
		return nil, fmt.Errorf("gateway: Config.Push: %w", err)
	}
	if err := checkPolicy(cfg.Pull.Push); err != nil {
		return nil, fmt.Errorf("gateway: Config.Pull.Push: %w", err)
	}
	for i, t := range cfg.Pull.Targets {
		if want, err := git.ParseRepo(t.String()); err != nil || want != t {
			return nil, fmt.Errorf("gateway: Config.Pull.Targets[%d] の形が正しくない", i)
		}
	}
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("gateway: Config.Credentials が nil")
	}
	gitBase, err := checkBaseURL(cfg.GitBaseURL, defaultGitBaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: GitBaseURL: %w", err)
	}
	apiBase, err := checkBaseURL(cfg.APIBaseURL, defaultAPIBaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: APIBaseURL: %w", err)
	}
	maxPack := cfg.MaxPackBytes
	if maxPack <= 0 {
		maxPack = DefaultMaxPackBytes
	}
	quota := cfg.PRQuota
	if quota <= 0 {
		quota = DefaultPRQuota
	}
	audit := cfg.Audit
	if audit == nil {
		audit = io.Discard
	}
	return &Handler{
		push: cfg.Push, pull: cfg.Pull, creds: cfg.Credentials,
		gitBase: gitBase, apiBase: apiBase, maxPack: maxPack,
		quota: github.NewQuota(quota), audit: audit, client: newUpstreamClient(),
	}, nil
}

// checkPolicy は、p の Repo・Session が、git.NewPolicy の検査を通るか確かめる (構造体リテラルで作った、検査を経ていない
// Policy を断る。攻撃者視点レビュー L2: CheckRef はリテラルの Policy も信用してしまうため、この Handler の入口で締める)。
// git.Policy は Repo と Session だけを持つので、NewPolicy(p.Repo, p.Session) が通れば、返る値は必ず p と同じになる
// (値を比べ直す意味は無い。通るかどうかだけを見る)。
func checkPolicy(p git.Policy) error {
	if _, err := git.NewPolicy(p.Repo, p.Session); err != nil {
		return fmt.Errorf("git.NewPolicy を通らない: %w", err)
	}
	return nil
}

// checkBaseURL は、u (空なら def) が、scheme://host の形で、末尾に / が無いことを確かめる。
func checkBaseURL(u, def string) (string, error) {
	if u == "" {
		u = def
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.Path != "" || p.RawQuery != "" || p.Fragment != "" {
		return "", fmt.Errorf("%q が http(s)://host の形ではない", u)
	}
	return u, nil
}

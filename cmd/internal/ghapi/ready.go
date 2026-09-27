package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nananek/goronation/core/credential"
)

// DefaultBaseURL は、GitHub.com の GraphQL エンドポイント。
const DefaultBaseURL = "https://api.github.com/graphql"

// maxResponseBytes は、応答を読む量の上限 (GraphQL の応答は、小さい JSON のはず)。
const maxResponseBytes = 1 << 20

// Client は、この package が実装する操作だけの、GitHub GraphQL API への呼び出し口。
type Client struct {
	// HTTPClient は、要求に使う *http.Client。nil なら http.DefaultClient。
	HTTPClient *http.Client
	// BaseURL は、GraphQL エンドポイント。空なら DefaultBaseURL (テストは、偽の上流を指定する)。
	BaseURL string
	// Token は、Authorization: bearer <Token> に使う。
	Token credential.Secret
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

// CheckState は、PR の head commit の、状態検査の集約結果 (StatusCheckRollup.state)。空文字は、
// checks が 1 つも設定されていない (rollup が無い) こと。
type CheckState string

// GitHub の StatusCheckRollup.state が取る値。
const (
	CheckSuccess  CheckState = "SUCCESS"
	CheckPending  CheckState = "PENDING"
	CheckFailure  CheckState = "FAILURE"
	CheckError    CheckState = "ERROR"
	CheckExpected CheckState = "EXPECTED"
)

// ErrChecksNotGreen は、head commit の checks が SUCCESS でない (まだ終わっていない・失敗した) ときの error。
var ErrChecksNotGreen = errors.New("ghapi: head の checks が success ではない")

// PullRequest は、Ready が要る PR の状態だけ。
type PullRequest struct {
	// NodeID は、GraphQL の node ID (mutation に渡す)。
	NodeID  string
	IsDraft bool
	HeadOID string
	// CheckState は、head commit の StatusCheckRollup.state。空なら、rollup が無い。
	CheckState CheckState
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphQLErrors は、GraphQL の応答の "errors" 配列。Error() は、最初の 1 件の message。
type graphQLErrors []struct {
	Message string `json:"message"`
}

func (es graphQLErrors) Error() string {
	if len(es) == 0 {
		return "graphql: 不明な error"
	}
	msgs := make([]string, len(es))
	for i, e := range es {
		msgs[i] = e.Message
	}
	return strings.Join(msgs, "; ")
}

// do は、query・variables を送り、応答の data を dst に decode する (dst が nil なら、data を読み捨てる)。
// HTTP が 200 以外、応答に errors がある、応答が大きすぎる・JSON として読めない、のいずれかなら error。
func (c *Client) do(ctx context.Context, query string, variables map[string]any, dst any) error {
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("ghapi: 要求を組み立てられない: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ghapi: 要求を作れない: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "bearer "+c.Token.Reveal())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("ghapi: GitHub に繋げない: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("ghapi: 応答を読めない: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return fmt.Errorf("ghapi: 応答が大きすぎる (上限 %d バイト)", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ghapi: GitHub が状態 %d を返した", resp.StatusCode)
	}
	var v struct {
		Data   json.RawMessage `json:"data"`
		Errors graphQLErrors   `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &v); err != nil {
		return fmt.Errorf("ghapi: 応答を JSON として読めない: %w", err)
	}
	if len(v.Errors) > 0 {
		return fmt.Errorf("ghapi: %w", v.Errors)
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(v.Data, dst); err != nil {
		return fmt.Errorf("ghapi: 応答の data を読めない: %w", err)
	}
	return nil
}

// pullRequestQuery は、PR の node ID・draft かどうか・head の checks の集約結果を取る。
const pullRequestQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      id
      isDraft
      headRefOid
      commits(last: 1) {
        nodes {
          commit {
            statusCheckRollup {
              state
            }
          }
        }
      }
    }
  }
}`

// getPullRequest は、owner/repo の PR number の、今の状態を取る。PR (か repo) が無ければ error。
func (c *Client) getPullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var v struct {
		Repository *struct {
			PullRequest *struct {
				ID         string `json:"id"`
				IsDraft    bool   `json:"isDraft"`
				HeadRefOID string `json:"headRefOid"`
				Commits    struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								State string `json:"state"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.do(ctx, pullRequestQuery, map[string]any{"owner": owner, "repo": repo, "number": number}, &v); err != nil {
		return PullRequest{}, err
	}
	if v.Repository == nil || v.Repository.PullRequest == nil {
		return PullRequest{}, fmt.Errorf("ghapi: %s/%s#%d が見つからない", owner, repo, number)
	}
	pr := v.Repository.PullRequest
	out := PullRequest{NodeID: pr.ID, IsDraft: pr.IsDraft, HeadOID: pr.HeadRefOID}
	if len(pr.Commits.Nodes) > 0 {
		if rollup := pr.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
			out.CheckState = CheckState(rollup.State)
		}
	}
	return out, nil
}

// markReadyMutation は、PR を ready for review にする (draft を外す)。
const markReadyMutation = `mutation($id: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $id}) {
    pullRequest {
      id
      isDraft
    }
  }
}`

// markReadyForReview は、nodeID の PR を ready for review にする。
func (c *Client) markReadyForReview(ctx context.Context, nodeID string) error {
	return c.do(ctx, markReadyMutation, map[string]any{"id": nodeID}, nil)
}

// Ready は、owner/repo の PR number を、head commit の checks が SUCCESS (または、checks が 1 つも
// 設定されていない) のときだけ、ready for review にする。すでに draft でなければ、何もせず
// alreadyReady=true で成功にする。checks が終わっていない・失敗していれば、ErrChecksNotGreen を包んだ
// error (状態を含む) を返し、mutation は呼ばない。
func (c *Client) Ready(ctx context.Context, owner, repo string, number int) (alreadyReady bool, err error) {
	pr, err := c.getPullRequest(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if !pr.IsDraft {
		return true, nil
	}
	if pr.CheckState != "" && pr.CheckState != CheckSuccess {
		return false, fmt.Errorf("%w (head %s の状態: %s)", ErrChecksNotGreen, pr.HeadOID, pr.CheckState)
	}
	if err := c.markReadyForReview(ctx, pr.NodeID); err != nil {
		return false, err
	}
	return false, nil
}
